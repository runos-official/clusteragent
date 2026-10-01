// Command clusteragent is the RunOS in-cluster agent: a single pod that
// maintains an mTLS gRPC stream to the RunOS control plane and executes the
// instructions it receives (app builds, image pushes, SQL execution, cert and
// DNS01 management, CLI deploy/pull). It also runs a local webhook server for
// health checks and presigned tarball uploads.
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/agentstream/instructions"
	"github.com/runos-official/clusteragent/buildkitclient"
	"github.com/runos-official/clusteragent/certcache"
	"github.com/runos-official/clusteragent/datastore"
	"github.com/runos-official/clusteragent/dns01"
	"github.com/runos-official/clusteragent/drain"
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

	// Hold new work until the startup cleanup below has finished. The cleanup
	// closes the build rows of the previous process and sweeps its build pods.
	// Without the hold it could fail a row, or delete a pod, that a new build
	// just made.
	drain.Hold()

	// One-shot build cleanup: sweep orphaned per-build BuildKit pods (their
	// builds died with the previous agent process) and tear down the legacy
	// shared buildkitd daemon if this cluster still runs one.
	podSweepDone := make(chan struct{})
	go func() {
		defer close(podSweepDone)
		k8sClient, err := agentstream.NewK8sClient()
		if err != nil {
			log.Printf("Build startup cleanup skipped, no k8s client: %v", err)
			return
		}
		buildkitclient.StartupCleanup(k8sClient.GetClientset())
	}()

	// Close the build rows that the previous process left pending or busy. The
	// system database may not be ready yet, so this retries. It releases the
	// hold once the pod sweep above has also finished.
	go reconcileUntilDone(datastore.FailNonTerminalBuildKitJobs, func() {
		<-podSweepDone
		drain.Release()
	}, reconcileRetryEvery, reconcileMaxWait)

	// Start services
	go dns01.Start()
	go agentstream.Start()
	go webhook.Start()
	go webhook.StartUploadServer()

	// Run until Kubernetes asks the pod to stop. Then refuse new work, wait for
	// the running builds and jobs, and fail what is left after the timeout. The
	// Deployment's termination grace period is longer than the drain timeout.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	sig := <-stop
	timeout := drainTimeout()
	log.Printf("Received %s: draining for up to %s", sig, timeout)
	shutdown(drainWithProgress, datastore.FailNonTerminalBuildKitJobs, timeout)
}
