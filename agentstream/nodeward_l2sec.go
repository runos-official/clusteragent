package agentstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

	cert, err := tls.X509KeyPair(tlsData.TLSCert, tlsData.TLSKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load client cert: %v", err)
	}

	ca := x509.NewCertPool()
	if !ca.AppendCertsFromPEM(tlsData.CACert) {
		return nil, nil, fmt.Errorf("failed to append CA cert to pool")
	}

	connectionString := fmt.Sprintf("%s:9192", serverHost)
	log.Printf("Connecting to %s", connectionString)

	tlsConfig := &tls.Config{
		InsecureSkipVerify: false,
		ServerName:         serverHost,
		Certificates:       []tls.Certificate{cert},
		RootCAs:            ca,
		MinVersion:         tls.VersionTLS12,
	}

	attemptCount := 0
	conn, err := grpc.DialContext(ctx, connectionString,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithBlock(), // This will make the Dial wait until the connection is established
		grpc.WithDefaultServiceConfig(`{
            "serviceConfig": {
                "healthCheckConfig": {
                    "serviceName": ""
                },
                "retryPolicy": {
                    "MaxAttempts": 5,
                    "InitialBackoff": "0.5s",
                    "MaxBackoff": "30s",
                    "BackoffMultiplier": 1.5,
                    "RetryableStatusCodes": [
                        "UNAVAILABLE"
                    ]
                }
            }
        }`),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  500 * time.Millisecond,
				Multiplier: 1.5,
				Jitter:     0.2,
				MaxDelay:   10 * time.Second,
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
