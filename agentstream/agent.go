// Package agentstream maintains the agent's long-lived mTLS gRPC stream to the
// RunOS control plane (Nodeward / L1Sec-L2Sec): it dials, reconnects with
// backoff, dispatches each inbound instruction to its handler in the
// instructions subpackage, and sends replies back over the same stream.
package agentstream

import (
	"context"
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
	"time"
)

var (
	globalStream     grpc.BidiStreamingClient[l2sec.FromClusterAgent, l2sec.ToClusterAgent]
	globalStreamMu   sync.RWMutex
	pendingResponses sync.Map

	// Reconnection configuration
	maxReconnectAttempts = 10
	baseReconnectDelay   = 1 * time.Second
	maxReconnectDelay    = 60 * time.Second
)

type streamManager struct {
	client          l2sec.NodewardClient
	clusterAgentTLS *TLSData
	serverHost      string
	k8s             *K8sClient
}

func Start() {
	log.Println("Connecting to the RunOS servers...")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	k8s, err := NewK8sClient()
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	runosConfig, err := k8s.GetRunosConfig(ctx)
	if err != nil {
		log.Fatalf("Failed to get RunOS config: %v", err)
	}

	clusterTLSExists, err := k8s.ClusterAgentTLSExists(ctx)
	if err != nil {
		log.Fatalf("Failed to check for cluster agent TLS secret: %v", err)
	}

	var clusterAgentTLS *TLSData
	usedNodeAgentCredentials := false

	if !clusterTLSExists {
		log.Println("Cluster agent TLS secret not found, using node agent credentials to configure the cluster agent...")
		nodeAgentTLS, err := k8s.GetNodeAgentTLS(ctx)
		if err != nil {
			log.Fatalf("Failed to get node agent TLS: %v", err)
		}
		clusterAgentTLS, err = generateNewClusterAgentTLSCredentials(nodeAgentTLS, runosConfig.Server)
		if err != nil {
			log.Fatalf("Failed to generate new cluster agent TLS credentials: %v", err)
		}
		if err = k8s.SetClusterAgentTLS(ctx, clusterAgentTLS); err != nil {
			log.Fatalf("Failed to set cluster agent TLS: %v", err)
		}
		usedNodeAgentCredentials = true
	} else {
		log.Println("Cluster agent TLS secret found, using existing credentials...")
		clusterAgentTLS, err = k8s.GetClusterAgentTLS(ctx)
		if err != nil {
			log.Fatalf("Failed to get cluster agent TLS: %v", err)
		}
	}

	// Log certificate status at startup
	LogCertStatus(clusterAgentTLS)

	// Initial connection
	client, _, err := ConnectToServer(clusterAgentTLS, runosConfig.Server, ctx)
	if err != nil {
		log.Fatalf("Failed to connect to server: %v", err)
	}

	if usedNodeAgentCredentials {
		if err = k8s.DeleteNodeAgentTLS(ctx); err != nil {
			log.Printf("Failed to delete node agent TLS: %v", err)
		}
	}

	// Create stream manager
	sm := &streamManager{
		client:          client,
		clusterAgentTLS: clusterAgentTLS,
		serverHost:      runosConfig.Server,
		k8s:             k8s,
	}

	// Start daily certificate monitoring and auto-renewal
	go sm.startCertMonitor()

	// Start the main stream loop with reconnection logic
	sm.runWithReconnect()
}

func (sm *streamManager) runWithReconnect() {
	reconnectAttempts := 0

	for {
		err := sm.establishAndMaintainStream()

		if err == nil {
			// Clean exit
			return
		}

		reconnectAttempts++
		if reconnectAttempts > maxReconnectAttempts {
			log.Fatalf("Failed to reconnect after %d attempts, exiting", maxReconnectAttempts)
		}

		// Calculate backoff delay
		delay := calculateBackoff(reconnectAttempts)
		log.Printf("Stream error: %v. Reconnecting in %v (attempt %d/%d)", err, delay, reconnectAttempts, maxReconnectAttempts)
		time.Sleep(delay)

		// Try to reconnect
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		client, _, err := ConnectToServer(sm.clusterAgentTLS, sm.serverHost, ctx)
		cancel()

		if err != nil {
			log.Printf("Reconnection attempt %d failed: %v", reconnectAttempts, err)
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

func calculateBackoff(attempt int) time.Duration {
	delay := baseReconnectDelay * time.Duration(1<<uint(attempt-1))
	if delay > maxReconnectDelay {
		delay = maxReconnectDelay
	}
	return delay
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
