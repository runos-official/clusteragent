// Package agentstream maintains the agent's long-lived mTLS gRPC stream to the
// RunOS control plane (Nodeward / L1Sec-L2Sec): it dials, reconnects with
// backoff, dispatches each inbound instruction to its handler in the
// instructions subpackage, and sends replies back over the same stream.
package agentstream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/runos-official/clusteragent/agentstream/instructions"
	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"github.com/runos-official/clusteragent/commons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"io"
	"log"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

type streamManager struct {
	client          l2sec.NodewardClient
	conn            *grpc.ClientConn
	clusterAgentTLS *TLSData
	serverHost      string
	k8s             *K8sClient

	// heartbeatInterval is how often the stream sends a heartbeat. Zero means
	// defaultHeartbeatInterval; tests set it shorter.
	heartbeatInterval time.Duration
}

const defaultHeartbeatInterval = 5 * time.Second

// activeHeartbeats counts running heartbeat goroutines. There is at most one
// per live stream.
var activeHeartbeats atomic.Int32

// Start brings up the agent's control link. The whole bootstrap (k8s client,
// runos-config ConfigMap, TLS material, initial Nodeward connect) runs through
// bootstrap(), which retries transient failures with backoff instead of
// crash-looping the pod, then hands off to the indefinite reconnect loop.
func Start() {
	log.Println("Connecting to the RunOS servers...")

	sm := bootstrap()

	// Start daily certificate monitoring and auto-renewal
	go sm.startCertMonitor()

	// Start the main stream loop with reconnection logic
	sm.runWithReconnect()
}

// bootstrap performs the one-time startup handshake, retrying every transient
// failure with capped backoff until it succeeds. During cluster creation each
// dependency (API server, the installer-written ConfigMap/Secret, Nodeward,
// in-cluster DNS) comes up asynchronously, so a raw log.Fatalf here turns a few
// seconds of normal startup races into a CrashLoopBackOff with a cryptic Go
// fatal. We only ever fatal on a genuinely operator-actionable condition (a
// malformed cert already at rest in the secret), and then with a remediation
// hint. Each step gets its own fresh per-attempt timeout.
func bootstrap() *streamManager {
	// Step 1: k8s client. Only fails on a malformed in-cluster config /
	// missing service-account mount, which is an environment/RBAC problem, but
	// it can also race the kubelet projecting the token, so retry.
	k8s := retryStep("create Kubernetes client", func() (*K8sClient, error) {
		return NewK8sClient()
	})

	// Step 2: runos-config ConfigMap. The installer writes this; on a fresh
	// cluster it may not exist for the first few seconds.
	runosConfig := retryStep("read runos-config ConfigMap", func() (*RunosConfig, error) {
		ctx, cancel := context.WithTimeout(context.Background(), bootstrapStepTimeout)
		defer cancel()
		cfg, err := k8s.GetRunosConfig(ctx)
		if err != nil {
			return nil, err
		}
		if cfg.Server == "" {
			// Present-but-empty: the ConfigMap was created before the installer
			// finished populating it. Treat as not-ready and keep waiting.
			return nil, fmt.Errorf("runos-config present but 'server' not yet set: %w", errNotReadyYet)
		}
		return cfg, nil
	})

	// Step 3: obtain cluster-agent TLS material. Either the secret already
	// exists (reuse it) or we mint it from the node-agent credentials. Every
	// sub-step retries on transient; a malformed existing cert is fatal.
	clusterAgentTLS, usedNodeAgentCredentials := bootstrapTLS(k8s, runosConfig.Server)

	// Log certificate status at startup.
	LogCertStatus(clusterAgentTLS)

	// Step 4: initial connection. Nodeward may still be warming up or DNS may
	// not resolve yet; both are retryable.
	type connected struct {
		client l2sec.NodewardClient
		conn   *grpc.ClientConn
	}
	first := retryStep("connect to Nodeward", func() (connected, error) {
		ctx, cancel := context.WithTimeout(context.Background(), bootstrapConnectTimeout)
		defer cancel()
		c, conn, err := ConnectToServer(clusterAgentTLS, runosConfig.Server, ctx)
		return connected{client: c, conn: conn}, err
	})

	if usedNodeAgentCredentials {
		// Best-effort cleanup; failure here doesn't block bootstrap.
		ctx, cancel := context.WithTimeout(context.Background(), bootstrapStepTimeout)
		if err := k8s.DeleteNodeAgentTLS(ctx); err != nil {
			log.Printf("Failed to delete node agent TLS (non-fatal): %v", err)
		}
		cancel()
	}

	return &streamManager{
		client:          first.client,
		conn:            first.conn,
		clusterAgentTLS: clusterAgentTLS,
		serverHost:      runosConfig.Server,
		k8s:             k8s,
	}
}

// errNotReadyYet marks a dependency that exists but is not yet fully populated
// (e.g. the ConfigMap created before its keys are set). isRetryable treats it
// as retryable via its default path.
var errNotReadyYet = errors.New("dependency not ready yet")

// bootstrapTLS returns the cluster-agent TLS material, retrying transient
// failures. If the secret already exists but is malformed it fatals with a
// remediation hint (the only operator-actionable bootstrap failure).
func bootstrapTLS(k8s *K8sClient, server string) (*TLSData, bool) {
	exists := retryStep("check cluster-agent-tls secret", func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), bootstrapStepTimeout)
		defer cancel()
		return k8s.ClusterAgentTLSExists(ctx)
	})

	if exists {
		log.Println("Cluster agent TLS secret found, using existing credentials...")
		// Read the secret, retrying while it is present-but-empty (the installer
		// can create the Secret object a moment before it populates tls.crt /
		// tls.key, so empty values are "not ready yet", not "malformed").
		clusterAgentTLS := retryStep("read cluster-agent-tls secret", func() (*TLSData, error) {
			ctx, cancel := context.WithTimeout(context.Background(), bootstrapStepTimeout)
			defer cancel()
			td, err := k8s.GetClusterAgentTLS(ctx)
			if err != nil {
				return nil, err
			}
			if td == nil || len(td.TLSCert) == 0 || len(td.TLSKey) == 0 {
				return nil, fmt.Errorf("cluster-agent-tls present but tls.crt/tls.key not populated yet: %w", errNotReadyYet)
			}
			return td, nil
		})
		// The bytes are non-empty. If they don't parse as a cert/key pair the
		// secret is genuinely malformed and waiting can't fix it: fatal with a
		// remediation hint instead of looping forever or panicking later inside
		// ConnectToServer's tls.X509KeyPair.
		if err := validateTLSData(clusterAgentTLS); err != nil {
			log.Fatalf("cluster-agent-tls secret is present but unusable: %v. "+
				"Remediation: delete the secret so it is regenerated, e.g. "+
				"`kubectl -n %s delete secret %s`, then let this pod restart.",
				err, Namespace, ClusterAgentTLSSecret)
		}
		return clusterAgentTLS, false
	}

	log.Println("Cluster agent TLS secret not found, using node agent credentials to configure the cluster agent...")
	nodeAgentTLS := retryStep("read node-agent-tls secret", func() (*TLSData, error) {
		ctx, cancel := context.WithTimeout(context.Background(), bootstrapStepTimeout)
		defer cancel()
		return k8s.GetNodeAgentTLS(ctx)
	})
	clusterAgentTLS := retryStep("generate cluster-agent TLS via Nodeward", func() (*TLSData, error) {
		return generateNewClusterAgentTLSCredentials(nodeAgentTLS, server)
	})
	retryStepVoid("write cluster-agent-tls secret", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), bootstrapStepTimeout)
		defer cancel()
		return k8s.SetClusterAgentTLS(ctx, clusterAgentTLS)
	})
	return clusterAgentTLS, true
}

// validateTLSData confirms the secret's bytes parse as a usable cert/key pair
// and CA pool. Returns errMalformedCert (the fatal sentinel) on bad bytes so
// the caller can distinguish "operator must fix this" from a transient.
func validateTLSData(tlsData *TLSData) error {
	if tlsData == nil || len(tlsData.TLSCert) == 0 || len(tlsData.TLSKey) == 0 {
		return fmt.Errorf("%w: missing tls.crt or tls.key", errMalformedCert)
	}
	if _, err := tls.X509KeyPair(tlsData.TLSCert, tlsData.TLSKey); err != nil {
		return fmt.Errorf("%w: %v", errMalformedCert, err)
	}
	return nil
}

// retryStep runs op until it returns a non-retryable result, retrying every
// retryable error (per isRetryable) with capped exponential backoff and a
// THROTTLED-style log line. A non-retryable error is fatal (op is responsible
// for wrapping such conditions, e.g. as errMalformedCert) — but in practice the
// only fatal bootstrap path is handled inline (malformed cert), so a
// non-retryable error reaching here is a programming error and we fatal with
// the underlying message rather than spin.
func retryStep[T any](name string, op func() (T, error)) T {
	return retryStepWith(name, op, time.Sleep, nil)
}

// retryStepWith is retryStep with the sleep and the random source injected.
// randInt63n nil means math/rand/v2.
func retryStepWith[T any](name string, op func() (T, error), sleep func(time.Duration), randInt63n func(int64) int64) T {
	attempt := 0
	for {
		attempt++
		val, err := op()
		if err == nil {
			if attempt > 1 {
				log.Printf("bootstrap: %s succeeded after %d attempts", name, attempt)
			}
			return val
		}
		if !isRetryable(err) {
			log.Fatalf("bootstrap: %s failed unrecoverably: %v", name, err)
		}
		delay := fullJitter(nextBackoff(attempt), randInt63n)
		log.Printf("THROTTLED bootstrap: %s not ready (attempt %d): %v; retrying in %s",
			name, attempt, err, delay)
		sleep(delay)
	}
}

// retryStepVoid is retryStep for an op with no return value.
func retryStepVoid(name string, op func() error) {
	retryStep(name, func() (struct{}, error) {
		return struct{}{}, op()
	})
}

// runWithReconnect maintains the control stream forever. A dropped stream or a
// failed reconnect is a transient: the agent backs off and retries
// indefinitely, it NEVER exits. The control link being down is not a reason to
// kill a pod that is otherwise healthy and serving its other roles (uploads,
// webhook); the operator should never have to restart it to recover from a
// blip. Disconnection is surfaced via the health endpoint (StreamConnected),
// not by crashing.
func (sm *streamManager) runWithReconnect() {
	sm.reconnectLoop(reconnectDeps{
		establish: sm.establishAndMaintainStream,
		connect: func(ctx context.Context) (l2sec.NodewardClient, *grpc.ClientConn, error) {
			return ConnectToServer(sm.clusterAgentTLS, sm.serverHost, ctx)
		},
		sleep:   time.Sleep,
		now:     time.Now,
		backoff: newReconnectBackoff(nil),
	})
}

// reconnectDeps are the parts of the reconnect loop a test swaps out.
type reconnectDeps struct {
	establish func() error
	connect   func(ctx context.Context) (l2sec.NodewardClient, *grpc.ClientConn, error)
	sleep     func(time.Duration)
	now       func() time.Time
	backoff   *reconnectBackoff
}

func (sm *streamManager) reconnectLoop(d reconnectDeps) {
	bo := d.backoff

	for {
		// Only the time the stream was up counts towards the reset, not the
		// sleep or the dial before it.
		started := d.now()
		err := d.establish()

		if err == nil {
			// Clean exit (e.g. server asked us to stop).
			return
		}

		// Full jitter, no attempt ceiling. The schedule starts again only
		// after the stream stayed up a while (FPL315).
		bo.connectionEnded(d.now().Sub(started))
		delay := bo.next()
		log.Printf("THROTTLED stream disconnected: %v. Reconnecting in %s (attempt %d)", err, delay, bo.attempt)
		d.sleep(delay)

		// Try to reconnect. A fresh per-attempt timeout, never a shared budget.
		ctx, cancel := context.WithTimeout(context.Background(), bootstrapConnectTimeout)
		client, conn, connErr := d.connect(ctx)
		cancel()

		if connErr != nil {
			log.Printf("Reconnection attempt %d failed: %v", bo.attempt, connErr)
			continue
		}

		// Close the replaced connection so reconnects do not leak them.
		if sm.conn != nil {
			_ = sm.conn.Close()
		}
		sm.client = client
		sm.conn = conn
		log.Println("Successfully reconnected to server")
	}
}

// onConnectCallback is called once after the first successful stream connection
var onConnectCallback func()
var onConnectOnce sync.Once

// SetOnConnectCallback sets a callback to be called once after the stream is established
func SetOnConnectCallback(callback func()) {
	onConnectCallback = callback
}

func (sm *streamManager) establishAndMaintainStream() error {
	// Each stream gets its own context. Cancelling it on the way out stops
	// this stream's heartbeat and ends a Recv still blocked on it, so a
	// dropped stream leaves no goroutine behind (FPL315).
	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	stream, err := sm.client.ClusterAgentStream(streamCtx)
	if err != nil {
		return fmt.Errorf("failed to create stream: %w", err)
	}

	return sm.runStream(streamCtx, stream, nil)
}

// runStream publishes stream as the one live Lifetime for its duration (L1
// publication), runs its heartbeat and listen goroutines, and returns the
// error produced once one of them ends that Lifetime (L5 invalidation).
//
// pauseCleanup, if non-nil, is received from right after that error is
// known and before the deferred lt.pending drain below, so a test
// (review finding 1313's regression) can hold the drain back indefinitely
// while still observing that every Lifetime-gated behavior - SendToServer,
// sendAndWait, a recheck permit - already reflects the ended stream, with
// no dependency on this drain having run at all. nil in production.
func (sm *streamManager) runStream(streamCtx context.Context, stream grpc.BidiStreamingClient[l2sec.FromClusterAgent, l2sec.ToClusterAgent], pauseCleanup <-chan struct{}) error {
	lt := newLifetime(stream)
	currentLifetime.Store(lt)

	// Run onConnect callback once
	if onConnectCallback != nil {
		onConnectOnce.Do(func() {
			go onConnectCallback()
		})
	}

	// Start goroutines
	errChan := make(chan error, 2)

	go func() {
		errChan <- sm.listenForInstructions(stream, lt)
	}()

	go func() {
		errChan <- sm.runHeartbeat(streamCtx, lt)
	}()

	// Wait for any error
	err := <-errChan

	if pauseCleanup != nil {
		<-pauseCleanup
	}

	// Clear THIS stream's own pending waiters on disconnect (review
	// finding 1403: draining a package-wide map here could close a
	// successor's still-pending waiter if this cleanup ran late; lt.pending
	// belongs to no other Lifetime, so this can never happen now).
	lt.pending.Range(func(key, value any) bool {
		if respChan, ok := value.(chan *l2sec.ToClusterAgent); ok {
			close(respChan)
		}
		lt.pending.Delete(key)
		return true
	})

	return err
}

func (sm *streamManager) listenForInstructions(stream grpc.BidiStreamingClient[l2sec.FromClusterAgent, l2sec.ToClusterAgent], lt *Lifetime) error {
	for {
		instruction, err := stream.Recv()
		if err != nil {
			// L5: end this stream's own Lifetime now, in this goroutine, at
			// the moment the closure is actually observed - before any log
			// write or return for the manager's own deferred cleanup.
			lt.end()
			if err == io.EOF {
				return fmt.Errorf("server closed the stream")
			}
			if status.Code(err) == codes.Canceled {
				return fmt.Errorf("stream was canceled")
			}
			return fmt.Errorf("error receiving instruction: %w", err)
		}

		// Process the instruction in a separate goroutine, inside a recover
		// boundary so a panic in any handler can't crash the whole agent pod.
		// lt is this stream's own Lifetime: a reply is dispatched only
		// against the waiter map it was registered in (review finding
		// 1403), never a different stream's.
		go safeHandleInstruction(instruction, lt)
	}
}

// runHeartbeat heartbeats on the stream it was started for until ctx ends or
// three heartbeats in a row fail. It never sends on a later stream: every
// send goes through sendOnLifetime(lt, ...), bound to lt.stream, never to
// whatever agentstream.CurrentLifetime() happens to be at send time.
func (sm *streamManager) runHeartbeat(ctx context.Context, lt *Lifetime) error {
	activeHeartbeats.Add(1)
	defer activeHeartbeats.Add(-1)
	interval := sm.heartbeatInterval
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	consecutiveFailures := 0
	maxConsecutiveFailures := 3

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			hbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			// sendAndWait takes lt explicitly and sends through
			// sendOnLifetime(lt, ...) - this stream's own Lifetime, never
			// "whatever is current" (review finding 1403: heartbeat used
			// to bypass the liveness check sendOnLifetime now applies to
			// every sender).
			res, _, err := sendAndWait(hbCtx, lt, sendOnLifetime, instructions.ClusterAgentHeartbeatToServer())
			cancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				consecutiveFailures++

				if consecutiveFailures >= maxConsecutiveFailures {
					// L5 (review finding 1404): end this stream's own
					// Lifetime BEFORE logging the terminal failure, not
					// after. log.Printf can block (a contended log
					// writer, rotation); ending first means the gap
					// finding 1270/1313 closed for receive-failure
					// detection cannot reopen here if that write stalls.
					lt.end()
					log.Printf("Heartbeat failed (%d/%d): %v", consecutiveFailures, maxConsecutiveFailures, err)
					return fmt.Errorf("heartbeat failed %d times consecutively", maxConsecutiveFailures)
				}
				log.Printf("Heartbeat failed (%d/%d): %v", consecutiveFailures, maxConsecutiveFailures, err)
				continue
			}

			consecutiveFailures = 0
			log.Printf("Received heartbeat response: %s", res.Type)
		}
	}
}

// SendToServer sends msg on the current control stream (L1 publication, L2
// send) through sendOnLifetime: it refuses rather than select a stream
// whose Lifetime has ended, even if the manager's own cleanup for that
// stream has not run yet.
func SendToServer(msg *l2sec.FromClusterAgent) error {
	lt := currentLifetime.Load()
	if !lt.Live() {
		return fmt.Errorf("stream not initialized")
	}

	return sendOnLifetime(lt, msg)
}

// sendMu serializes every Send call on the stream. gRPC does not allow
// concurrent Send calls on the same ClientStream; the heartbeat ticker and
// any instruction reply or outbound request (including a deployment stage
// recheck) otherwise race here with no ordering guarantee (review finding
// 1077). The agent has exactly one active bidi stream at a time - a
// reconnect replaces currentLifetime wholesale - so one mutex for the whole
// package is the stream's own serialization point.
var sendMu sync.Mutex

// sendOnLifetime sends msg on lt's own stream (L2 send), the single path
// every sender uses now - SendToServer, the heartbeat (review finding
// 1403: it used to call stream.Send directly, bypassing any liveness
// check), and sendAndWait's registered request alike.
//
// It checks Live() twice: once before contending for sendMu (a cheap, not
// strictly necessary short-circuit), and again immediately after
// acquiring it (review finding 1403: "check the lifetime after waiting
// for send serialization"). A send queued behind another writer can have
// its Lifetime end while it waits for the mutex; without the second
// check, it would still transmit on a now-dead stream once its turn
// comes. Checking Live() and calling Send() while still holding sendMu
// closes that gap: nothing between "lt is live" and "the bytes are on the
// wire" can un-live it, because end() never blocks on sendMu itself.
func sendOnLifetime(lt *Lifetime, msg *l2sec.FromClusterAgent) error {
	if !lt.Live() {
		return fmt.Errorf("stream not initialized")
	}

	if msg.Tag == "" {
		msg.Tag = uuid.New().String()
	}

	sendMu.Lock()
	defer sendMu.Unlock()
	if !lt.Live() {
		return fmt.Errorf("control stream disconnected before this send could be serialized")
	}
	err := lt.stream.Send(msg)
	if err != nil {
		log.Printf("Error sending message to server: %v", err)
		return err
	}
	log.Printf("Sent message to server type:%s tag:%s", msg.Type, msg.Tag)
	return nil
}

func validateUUID(id string) (uuid.UUID, error) {
	return uuid.Parse(id)
}

// safeHandleInstruction wraps handleInstruction in a recover boundary so a panic
// in ANY handler can never unwind the goroutine and crash the agent pod. The
// cluster agent is a single per-cluster process with no worker-pool isolation,
// so one unrecovered panic would CrashLoopBackOff the entire control surface
// (uploads, webhooks, builds, SQL, Harbor). On panic it logs the value + stack
// and replies with an error for the instruction's tag (so the caller is not left
// hanging), then returns; the stream loop keeps serving. Mirrors the node
// agent's stream.go safeHandleInstruction.
func safeHandleInstruction(instruction *l2sec.ToClusterAgent, lt *Lifetime) {
	defer func() {
		if r := recover(); r != nil {
			// Defensive: the panic may itself have come from a nil/garbage
			// instruction, so the recovery path must never panic again.
			var tag string
			if instruction != nil {
				tag = instruction.Tag
			}
			log.Printf("Recovered from panic handling instruction (tag %s): %v\n%s", tag, r, string(debug.Stack()))
			if tagID, err := validateUUID(tag); err == nil {
				respondWithErr(fmt.Sprintf("internal error handling instruction: %v", r), tagID)
			}
		}
	}()
	handleInstruction(instruction, lt)
}

// handleInstruction dispatches one instruction received on lt's stream.
// lt is this stream's own Lifetime, so a reply is matched only against
// the waiter that registered under lt.pending (review finding 1403: a
// package-wide map let a reply arrive, and be dispatched, against a
// DIFFERENT lifetime than the one the waiter actually sent on - correctly
// correlated only by tag, which is not enough once more than one
// Lifetime can exist at once). Nothing routes a reply received on one
// concrete stream into another stream's waiter map.
func handleInstruction(instruction *l2sec.ToClusterAgent, lt *Lifetime) {
	tag := instruction.Tag
	tagID, err := validateUUID(tag)
	if err != nil {
		log.Printf("Received instruction with invalid UUID tag: %v (ignoring)", err)
		return
	}

	log.Printf("Received instruction with tag %s of type: %s", tagID.String(), instruction.Type)

	if respChanValue, exists := lt.pending.Load(tagID.String()); exists {
		responseChan := respChanValue.(chan *l2sec.ToClusterAgent)
		select {
		case responseChan <- instruction:
			return
		default:
			log.Printf("Warning: Could not deliver response for tag %s", tagID.String())
		}
		lt.pending.Delete(tagID.String())
		return
	}

	handler, exists := instructions.Map[instruction.Type]
	if !exists {
		handler, exists = streamingHandler(instruction)
	}
	if !exists {
		respondWithErr(fmt.Sprintf("No handler found for message type: %s", instruction.Type), tagID)
		return
	}

	start := time.Now()
	replyType, jsonB64, err := handler(instruction.JsonB64)
	duration := time.Since(start).Milliseconds()

	if err != nil {
		log.Printf("Error processing %s in %dms: %v", instruction.Type, duration, err)
		respondWithErr(fmt.Sprintf("Error processing %s: %v", instruction.Type, err), tagID)
		return
	}

	if replyType == "" {
		replyType = "ACK"
	}

	log.Printf("Processed %s in %dms", instruction.Type, duration)
	respond(jsonB64, replyType, tagID)
}

func respondWithErr(errMsg string, tagID uuid.UUID) {
	log.Printf("Responding with error: %s on tag %s", errMsg, tagID)
	type errorResponse struct {
		ErrMsg string `json:"errMsg"`
	}
	var errResponse errorResponse
	errResponse.ErrMsg = errMsg
	jsonB64, err := commons.JsonB64Encode(errResponse)
	if err != nil {
		log.Printf("Error encoding message: %v", err)
	}

	respond(jsonB64, "ERROR", tagID)
}

func respond(jsonB64, replyType string, tagID uuid.UUID) {
	response := &l2sec.FromClusterAgent{
		JsonB64: jsonB64,
		Type:    replyType,
		Tag:     tagID.String(),
	}

	err := SendToServer(response)
	if err != nil {
		log.Printf("Error sending response: %v", err)
	}
}

func SendAndWaitForResponse(ctx context.Context, msg *l2sec.FromClusterAgent) (*l2sec.ToClusterAgent, error) {
	resp, _, err := sendAndWaitOnCurrent(ctx, msg)
	return resp, err
}

// SendAndWaitForResponseWithLifetime is SendAndWaitForResponse plus the
// Lifetime the reply was validated against (L3). The published
// deployment-stage-recheck contract (deploymentrecheck.go) is the only
// caller that needs to bind further authority - a permit - to this exact
// connection (L4); every other caller uses SendAndWaitForResponse and
// ignores it.
func SendAndWaitForResponseWithLifetime(ctx context.Context, msg *l2sec.FromClusterAgent) (*l2sec.ToClusterAgent, *Lifetime, error) {
	return sendAndWaitOnCurrent(ctx, msg)
}

// sendAndWaitOnCurrent samples currentLifetime exactly once and carries
// that single value through registration and send (review finding 1403:
// sendAndWait used to sample it once for registration, then hand off to a
// goroutine that called SendToServer, which sampled currentLifetime AGAIN
// - a reconnect in the gap between the two samples could make the second
// send actually transmit on a brand new successor stream while the first,
// stale sample still governed whether the reply was accepted, rejecting a
// reply the successor legitimately delivered). Sampling once here and
// passing it to sendAndWait closes that gap: whichever Lifetime is
// current at the moment this call starts is the only one this call will
// ever register under or send through.
func sendAndWaitOnCurrent(ctx context.Context, msg *l2sec.FromClusterAgent) (*l2sec.ToClusterAgent, *Lifetime, error) {
	lt := currentLifetime.Load()
	if !lt.Live() {
		return nil, nil, fmt.Errorf("stream not initialized")
	}
	return sendAndWait(ctx, lt, sendOnLifetime, msg)
}

// sendAndWait sends msg on lt via send (sendOnLifetime in production) and
// waits for the reply on its tag, registered under lt.pending - never a
// package-wide map (review finding 1403), so this exact Lifetime owns
// both the registration and the cleanup that can later drain it. It
// returns lt itself once the reply is accepted, still live: the one
// Lifetime carried through selection, registration, transport send,
// receive dispatch, and waiter lookup, with no resampling at any step.
func sendAndWait(ctx context.Context, lt *Lifetime, send func(*Lifetime, *l2sec.FromClusterAgent) error, msg *l2sec.FromClusterAgent) (*l2sec.ToClusterAgent, *Lifetime, error) {
	tagID := uuid.New()
	msg.Tag = tagID.String()

	responseChan := make(chan *l2sec.ToClusterAgent, 1)
	lt.pending.Store(tagID.String(), responseChan)
	defer lt.pending.Delete(tagID.String())

	// Review finding 1077: send used to run synchronously and unbounded by
	// ctx. gRPC flow control can stall stream.Send past any caller deadline
	// (RequestStageRecheck's five-second ceiling included), so a stalled
	// sender used to block the whole call well past its own timeout. Run it
	// in a goroutine and race it against ctx here too, not only in the
	// reply wait below, so a bounded deadline is actually bounded. If ctx
	// wins, the deferred lt.pending.Delete above still runs and the send
	// goroutine, if it later succeeds, writes into sendDone with nobody
	// left to read it - a harmless buffered write, not a leak.
	sendDone := make(chan error, 1)
	go func() { sendDone <- send(lt, msg) }()

	select {
	case err := <-sendDone:
		if err != nil {
			return nil, nil, fmt.Errorf("failed to send message: %w", err)
		}
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}

	select {
	case response, ok := <-responseChan:
		if !ok {
			return nil, nil, fmt.Errorf("response channel was closed unexpectedly")
		}
		// L3: a reply already buffered for this tag (agent.go's
		// handleInstruction writes it into this capacity-one channel and
		// returns) can still be read successfully here even after the
		// stream it arrived on has since ended - a close on a buffered
		// channel does not discard a value already written to it. Refuse
		// it unless the Lifetime sampled above is still live now.
		if !lt.Live() {
			return nil, nil, fmt.Errorf("control stream disconnected or reconnected while the reply was in flight")
		}
		return response, lt, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func SendAndWaitForResponseWithTimeout(msg *l2sec.FromClusterAgent, timeout time.Duration) (*l2sec.ToClusterAgent, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return SendAndWaitForResponse(ctx, msg)
}

func generateNewClusterAgentTLSCredentials(nodeAgentTLS *TLSData, serverHost string) (*TLSData, error) {
	credCtx, credCancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer credCancel()

	client, conn, err := ConnectToServer(nodeAgentTLS, serverHost, credCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to server: %w", err)
	}
	defer conn.Close()

	log.Println("Successfully connected to server with node agent TLS credentials")

	// The cluster agent makes its own key here and sends a CSR. The proof is
	// signed with the node agent key it connected with.
	tlsData, err := requestClusterAgentCredentials(credCtx, client, nodeAgentTLS)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster agent credentials: %w", err)
	}

	log.Println("Successfully received cluster agent TLS credentials")

	return tlsData, nil
}
