// Command clusteragent is the RunOS in-cluster agent: a single pod that
// maintains an mTLS gRPC stream to the RunOS control plane and executes the
// instructions it receives (app builds, image pushes, SQL execution, cert and
// DNS01 management, CLI deploy/pull). It also runs a local webhook server for
// health checks and presigned tarball uploads.
package main

import (
	"log"
	"time"

	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/agentstream/instructions"
	"github.com/runos-official/clusteragent/buildkitclient"
	"github.com/runos-official/clusteragent/certcache"
	"github.com/runos-official/clusteragent/datastore"
	"github.com/runos-official/clusteragent/dns01"
	"github.com/runos-official/clusteragent/sqlwrapper"
	"github.com/runos-official/clusteragent/version"
	"github.com/runos-official/clusteragent/webhook"
)

func main() {
	log.Printf("Starting the RunOS Cluster Agent v%s", version.Version)

	// Initialize datastore
	if err := datastore.Initialize(); err != nil {
		log.Fatalf("Failed to initialize datastore: %v", err)
	}
	defer datastore.Close()

	// Start expired token cleanup goroutine
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if err := datastore.DeleteExpiredUploadTokens(); err != nil {
				log.Printf("Failed to cleanup expired tokens: %v", err)
			}
		}
	}()

	// Start SQL connection pool cleanup goroutine
	go sqlwrapper.StartPoolCleanup()
	defer sqlwrapper.CloseAllPools()

	// Wire the VCS build executor (lives in webhook because it needs the
	// K8s/BuildKit setup; the instructions package can't import webhook
	// directly without creating a cycle through agentstream).
	instructions.VcsBuildExecutor = webhook.RunVcsBuild
	instructions.HarborImageExistsExecutor = webhook.HarborImageExists

	// Set up cert cache check to run after agentstream connects
	agentstream.SetOnConnectCallback(certcache.CheckAndRestoreClusterDomainCert)

	// One-shot build cleanup: sweep orphaned per-build BuildKit pods (their
	// builds died with the previous agent process) and tear down the legacy
	// shared buildkitd daemon if this cluster still runs one.
	go func() {
		k8sClient, err := agentstream.NewK8sClient()
		if err != nil {
			log.Printf("Build startup cleanup skipped, no k8s client: %v", err)
			return
		}
		buildkitclient.StartupCleanup(k8sClient.GetClientset())
	}()

	// Start services
	go dns01.Start()
	go agentstream.Start()
	go webhook.Start()
	go webhook.StartUploadServer()

	select {}
}
