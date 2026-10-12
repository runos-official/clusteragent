package agentstream

import (
	"context"
	"fmt"
	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"log"
	"time"
)

func ConnectToServer(tlsData *TLSData, serverHost string, ctx context.Context) (
	l2sec.NodewardClient, *grpc.ClientConn, error) {

	tlsConfig, err := serverTLSConfig(tlsData, serverHost)
	if err != nil {
		return nil, nil, err
	}

	connectionString := fmt.Sprintf("%s:9192", serverHost)
	log.Printf("Connecting to %s", connectionString)

	attemptCount := 0
	conn, err := grpc.DialContext(ctx, connectionString,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithBlock(), // This will make the Dial wait until the connection is established
		grpc.WithDefaultServiceConfig(nodewardServiceConfig),
		grpc.WithConnectParams(grpc.ConnectParams{
			// Transport retries inside one dial. Jitter 1 spreads each wait
			// over [0, 2x delay], so after the first retry the wait is full
			// jitter with a cap that doubles from 1 s up to 30 s.
			Backoff: backoff.Config{
				BaseDelay:  500 * time.Millisecond,
				Multiplier: 2,
				Jitter:     1,
				MaxDelay:   15 * time.Second,
			},
			MinConnectTimeout: 20 * time.Second,
		}),
		grpc.WithUnaryInterceptor(func(methodCtx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			attemptCount++
			log.Printf("Attempt %d: Trying to connect to %s", attemptCount, connectionString)

			err := invoker(methodCtx, method, req, reply, cc, opts...)
			if err != nil {
				log.Printf("Attempt %d failed: %v", attemptCount, err)
			} else {
				log.Printf("Attempt %d succeeded", attemptCount)
			}
			return err
		}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect: %v", err)
	}

	c := l2sec.NewNodewardClient(conn)
	return c, conn, nil
}

// nodewardServiceConfig retries UNAVAILABLE on idempotent reads only.
// RegenerateCertificate and GetClusterAgentCredentials each mint a
// certificate, and the other unary methods change nodeward state, so a retry
// of those could act twice. Neither listed method is called by this agent
// today; the policy covers them should it start to.
//
// The config before FPL315 nested everything under a "serviceConfig" key,
// which grpc-go ignores, so no retry policy ever applied.
const nodewardServiceConfig = `{
  "methodConfig": [{
    "name": [
      {"service": "runos.nodeward.proto.l2sec.Nodeward", "method": "GetControlPlaneNodes"},
      {"service": "runos.nodeward.proto.l2sec.Nodeward", "method": "GetInstallVpnCommands"}
    ],
    "retryPolicy": {
      "maxAttempts": 4,
      "initialBackoff": "0.5s",
      "maxBackoff": "5s",
      "backoffMultiplier": 2,
      "retryableStatusCodes": ["UNAVAILABLE"]
    }
  }]
}`
