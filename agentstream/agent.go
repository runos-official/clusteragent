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
	"sync"
	"sync/atomic"
	"time"
)

var (
	globalStream     grpc.BidiStreamingClient[l2sec.FromClusterAgent, l2sec.ToClusterAgent]
	globalStreamMu   sync.RWMutex
	pendingResponses sync.Map

	// streamConnected reflects whether the gRPC control stream is currently up.
	// The webhook /health handler reads it to surface "disconnected" in its
	// body without failing the liveness probe (see webhook.HandleHealth): a
	// disconnected agent is still alive and retrying, so it must not be killed.
	streamConnected atomic.Bool
)

// StreamConnected reports whether the agent currently holds a live gRPC stream
// to the control plane. Used by the webhook health endpoint to surface
// connection state without affecting liveness.
func StreamConnected() bool {
	return streamConnected.Load()
}

type streamManager struct {
	client          l2sec.NodewardClient
	clusterAgentTLS *TLSData
	serverHost      string
	k8s             *K8sClient
}

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
	client := retryStep("connect to Nodeward", func() (l2sec.NodewardClient, error) {
		ctx, cancel := context.WithTimeout(context.Background(), bootstrapConnectTimeout)
		defer cancel()
		c, _, err := ConnectToServer(clusterAgentTLS, runosConfig.Server, ctx)
		return c, err
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
		client:          client,
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
		delay := nextBackoff(attempt)
		log.Printf("THROTTLED bootstrap: %s not ready (attempt %d): %v; retrying in %s",
			name, attempt, err, delay)
		time.Sleep(delay)
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
	reconnectAttempts := 0

	for {
		err := sm.establishAndMaintainStream()

		if err == nil {
			// Clean exit (e.g. server asked us to stop).
			return
		}

		reconnectAttempts++

		// Capped exponential backoff, no attempt ceiling.
		delay := nextBackoff(reconnectAttempts)
		log.Printf("THROTTLED stream disconnected: %v. Reconnecting in %s (attempt %d)", err, delay, reconnectAttempts)
		time.Sleep(delay)

		// Try to reconnect. A fresh per-attempt timeout, never a shared budget.
		ctx, cancel := context.WithTimeout(context.Background(), bootstrapConnectTimeout)
		client, _, connErr := ConnectToServer(sm.clusterAgentTLS, sm.serverHost, ctx)
		cancel()

		if connErr != nil {
			log.Printf("Reconnection attempt %d failed: %v", reconnectAttempts, connErr)
			continue
		}

		sm.client = client
		reconnectAttempts = 0 // Reset on successful connection
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
	streamCtx := context.Background()
	stream, err := sm.client.ClusterAgentStream(streamCtx)
	if err != nil {
		return fmt.Errorf("failed to create stream: %w", err)
	}

	// Update global stream with mutex
	globalStreamMu.Lock()
	globalStream = stream
	globalStreamMu.Unlock()
	streamConnected.Store(true)

	// Run onConnect callback once
	if onConnectCallback != nil {
		onConnectOnce.Do(func() {
			go onConnectCallback()
		})
	}

	// Start goroutines
	errChan := make(chan error, 2)

	go func() {
		errChan <- sm.listenForInstructions(stream)
	}()

	go func() {
		errChan <- sm.runHeartbeat()
	}()

	// Wait for any error
	err = <-errChan

	// Clean up
	streamConnected.Store(false)
	globalStreamMu.Lock()
	globalStream = nil
	globalStreamMu.Unlock()

	// Clear pending responses on disconnect
	pendingResponses.Range(func(key, value any) bool {
		if respChan, ok := value.(chan *l2sec.ToClusterAgent); ok {
			close(respChan)
		}
		pendingResponses.Delete(key)
		return true
	})

	return err
}

func (sm *streamManager) listenForInstructions(stream grpc.BidiStreamingClient[l2sec.FromClusterAgent, l2sec.ToClusterAgent]) error {
	for {
		instruction, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("server closed the stream")
			}
			if status.Code(err) == codes.Canceled {
				return fmt.Errorf("stream was canceled")
			}
			return fmt.Errorf("error receiving instruction: %w", err)
		}

		// Process the instruction in a separate goroutine
		go handleInstruction(instruction)
	}
}

func (sm *streamManager) runHeartbeat() error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	consecutiveFailures := 0
	maxConsecutiveFailures := 3

	for {
		select {
		case <-ticker.C:
			res, err := SendAndWaitForResponseWithTimeout(instructions.ClusterAgentHeartbeatToServer(), 10*time.Second)
			if err != nil {
				consecutiveFailures++
				log.Printf("Heartbeat failed (%d/%d): %v", consecutiveFailures, maxConsecutiveFailures, err)

				if consecutiveFailures >= maxConsecutiveFailures {
					return fmt.Errorf("heartbeat failed %d times consecutively", maxConsecutiveFailures)
				}
				continue
			}

			consecutiveFailures = 0
			log.Printf("Received heartbeat response: %s", res.Type)
		}
	}
}

func SendToServer(msg *l2sec.FromClusterAgent) error {
	globalStreamMu.RLock()
	stream := globalStream
	globalStreamMu.RUnlock()

	if stream == nil {
		return fmt.Errorf("stream not initialized")
	}

	if msg.Tag == "" {
		msg.Tag = uuid.New().String()
	}

	err := stream.Send(msg)
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

func handleInstruction(instruction *l2sec.ToClusterAgent) {
	tag := instruction.Tag
	tagID, err := validateUUID(tag)
	if err != nil {
		log.Printf("Received instruction with invalid UUID tag: %v (ignoring)", err)
		return
	}

	log.Printf("Received instruction with tag %s of type: %s", tagID.String(), instruction.Type)

	if respChanValue, exists := pendingResponses.Load(tagID.String()); exists {
		responseChan := respChanValue.(chan *l2sec.ToClusterAgent)
		select {
		case responseChan <- instruction:
			return
		default:
			log.Printf("Warning: Could not deliver response for tag %s", tagID.String())
		}
		pendingResponses.Delete(tagID.String())
		return
	}

	handler, exists := instructions.Map[instruction.Type]
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
	tagID := uuid.New()
	msg.Tag = tagID.String()

	responseChan := make(chan *l2sec.ToClusterAgent, 1)
	pendingResponses.Store(tagID.String(), responseChan)
	defer pendingResponses.Delete(tagID.String())

	if err := SendToServer(msg); err != nil {
		return nil, fmt.Errorf("failed to send message: %w", err)
	}

	select {
	case response, ok := <-responseChan:
		if !ok {
			return nil, fmt.Errorf("response channel was closed unexpectedly")
		}
		return response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
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

	res, err := client.GetClusterAgentCredentials(credCtx, &l2sec.GetClusterAgentCredentialsRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster agent credentials: %w", err)
	}

	log.Println("Successfully received cluster agent TLS credentials")

	return &TLSData{
		TLSCert: []byte(res.PublicKey),
		TLSKey:  []byte(res.PrivateKey),
		CACert:  []byte(res.CaCert),
	}, nil
}
