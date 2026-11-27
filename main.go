// Command clusteragent is the RunOS in-cluster agent: a single pod that
// maintains an mTLS gRPC stream to the RunOS control plane and executes the
// instructions it receives (app builds, image pushes, SQL execution, cert and
// DNS01 management, CLI deploy/pull). It also runs a local webhook server for
// health checks and presigned tarball uploads.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/agentstream/instructions"
	"github.com/runos-official/clusteragent/buildkitclient"
	"github.com/runos-official/clusteragent/certcache"
	"github.com/runos-official/clusteragent/datastore"
	"github.com/runos-official/clusteragent/dns01"
	"github.com/runos-official/clusteragent/drain"
	"github.com/runos-official/clusteragent/siblings"
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

	// Wire the job-authority stage-recheck client (story 339). instructions
	// cannot import agentstream directly (agentstream already imports
	// instructions to dispatch), so this adapter is the one place that
	// converts the plain stage-name strings instructions uses into
	// agentstream's typed RecheckStage/JobAuthority call.
	instructions.RequestJobStagePermit = func(ctx context.Context, authorityJobID, stage string) error {
		resp, err := agentstream.RequestStageRecheck(ctx, agentstream.JobAuthority(authorityJobID), agentstream.RecheckStage(stage), uuid.New().String())
		if err != nil {
			return fmt.Errorf("deployment stage recheck unavailable: %w", err)
		}
		if resp.Outcome != agentstream.OutcomePermit {
			return fmt.Errorf("deployment stage recheck outcome %q (code %q)", resp.Outcome, resp.Code)
		}
		return nil
	}

	// Set up cert cache check to run after agentstream connects
	agentstream.SetOnConnectCallback(certcache.CheckAndRestoreClusterDomainCert)

	// Hold new work until the startup cleanup below has finished. The cleanup
	// closes the build rows of the previous process and sweeps its build pods.
	// Without the hold it could fail a row, or delete a pod, that a new build
	// just made.
	drain.Hold()

	// One-shot build cleanup, once no other agent pod runs (a rolling update
	// keeps the old pod running until this one is Ready). Sweep orphaned
	// per-build BuildKit pods (their builds died with the previous agent
	// process), tear down the legacy shared buildkitd daemon if this cluster
	// still runs one, then close the build rows that the previous process
	// left pending or busy. The system database may not be ready yet, so the
	// row cleanup retries. The hold is released at the end.
	k8sClient, k8sErr := agentstream.NewK8sClient()
	go startupCleanup(
		func() { waitUntilAlone(k8sClient, k8sErr) },
		func() {
			if k8sErr != nil {
				log.Printf("Build startup cleanup skipped, no k8s client: %v", k8sErr)
				return
			}
			buildkitclient.StartupCleanup(k8sClient.GetClientset())
		},
		datastore.FailNonTerminalBuildKitJobs, drain.Release,
		reconcileRetryEvery, reconcileMaxWait)

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

// waitUntilAlone blocks until no other agent pod runs, or until aloneMaxWait
// has passed. Without a Kubernetes client it returns at once, as before.
func waitUntilAlone(k8sClient *agentstream.K8sClient, k8sErr error) {
	if k8sErr != nil {
		return
	}
	self := siblings.SelfName()
	list := func() ([]siblings.Pod, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return siblings.List(ctx, k8sClient.GetClientset())
	}
	start := time.Now()
	if siblings.WaitUntilAlone(list, self, aloneCheckEvery, aloneMaxWait, time.Sleep, time.Now) {
		if waited := time.Since(start); waited > aloneCheckEvery {
			log.Printf("Build startup cleanup: the other agent pod has gone after %s", waited.Round(time.Second))
		}
		return
	}
	log.Printf("Build startup cleanup: another agent pod still runs after %s; cleaning up anyway", aloneMaxWait)
}
