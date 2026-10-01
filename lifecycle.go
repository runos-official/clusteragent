package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/runos-official/clusteragent/drain"
)

const (
	// defaultDrainTimeout is how long a shutdown waits for running builds and
	// one-shot jobs. It matches the 30 minute build budget that conductor
	// grants a build, plus margin. The Deployment's termination grace period
	// must stay above it, or Kubernetes kills the pod before the drain ends.
	defaultDrainTimeout = 35 * time.Minute

	// drainTimeoutEnv overrides the drain timeout with a Go duration, for
	// example "90s". An invalid value falls back to the default.
	drainTimeoutEnv = "CLUSTER_AGENT_DRAIN_TIMEOUT"

	// drainProgressEvery is how often a draining agent logs the work it waits for.
	drainProgressEvery = 30 * time.Second

	// reconcileRetryEvery and reconcileMaxWait bound the startup reconcile. The
	// system database may not be ready when the agent starts, so the reconcile
	// retries. After reconcileMaxWait the agent accepts work anyway: a failed
	// cleanup must not stop builds for ever.
	reconcileRetryEvery = 3 * time.Second
	reconcileMaxWait    = 2 * time.Minute

	reasonRestart = "build interrupted by agent restart: the previous agent process ended while the build ran"
)

// failRowsFunc closes every non-terminal build row with a reason. It returns
// how many rows it closed. datastore.FailNonTerminalBuildKitJobs implements it.
type failRowsFunc func(reason string) (int, error)

// drainTimeout returns the shutdown drain timeout.
func drainTimeout() time.Duration {
	v := os.Getenv(drainTimeoutEnv)
	if v == "" {
		return defaultDrainTimeout
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Printf("Ignoring invalid %s=%q, using %s", drainTimeoutEnv, v, defaultDrainTimeout)
		return defaultDrainTimeout
	}
	return d
}

// shutdown drains running work, then fails the build rows that are still open
// when the timeout passes. A clean drain leaves the rows alone: each build has
// closed its own row.
func shutdown(drainFn func(time.Duration) int, failRows failRowsFunc, timeout time.Duration) {
	remaining := drainFn(timeout)
	if remaining == 0 {
		log.Printf("Drain complete: no builds or jobs were running at shutdown")
		return
	}
	reason := fmt.Sprintf("build interrupted by agent shutdown: it still ran after the %s drain wait", timeout)
	log.Printf("Drain timed out with %d unit(s) of work still running; failing their build rows", remaining)
	closed, err := failRows(reason)
	if err != nil {
		log.Printf("Failed to close build rows at shutdown: %v", err)
		return
	}
	log.Printf("Closed %d build row(s) at shutdown", closed)
}

// drainWithProgress drains the process-wide tracker and logs the work it waits
// for, so a long drain does not look like a hang.
func drainWithProgress(timeout time.Duration) int {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(drainProgressEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				log.Printf("Draining: waiting for %d running build(s) or job(s)", drain.InFlight())
			}
		}
	}()
	return drain.Drain(timeout)
}

// reconcileOrphanedBuilds closes the build rows that the previous agent process
// left open. Call it only while no work is accepted: every open row then
// belongs to a process that no longer exists.
func reconcileOrphanedBuilds(failRows failRowsFunc) (int, error) {
	closed, err := failRows(reasonRestart)
	if err != nil {
		return 0, err
	}
	if closed > 0 {
		log.Printf("Startup reconcile closed %d build row(s) that the previous agent process left open", closed)
	}
	return closed, nil
}

// reconcileUntilDone runs the startup reconcile, retrying while the database is
// not ready, and then calls release so the agent accepts work. It calls release
// exactly once, also when the reconcile never succeeds within maxWait.
func reconcileUntilDone(failRows failRowsFunc, release func(), every, maxWait time.Duration) {
	start := time.Now()
	for {
		_, err := reconcileOrphanedBuilds(failRows)
		if err == nil {
			release()
			return
		}
		if time.Since(start) >= maxWait {
			log.Printf("Startup reconcile gave up after %s (%v); accepting work anyway, open build rows from the previous process stay open", maxWait, err)
			release()
			return
		}
		time.Sleep(every)
	}
}
