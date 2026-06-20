package instructions

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/runos-official/clusteragent/commons"
	"github.com/runos-official/clusteragent/datastore"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// RunOneShotJobRequest is the input for RUN_ONESHOT_JOB. The conductor owns
// build-on-demand and namespace env-prep, so by the time this arrives the
// image already exists in Harbor and the referenced ConfigMap + Secret are
// guaranteed present in the namespace. The cluster agent only has to run the
// one-shot Job and report the result.
//
// RunID is the conductor-supplied unique id for this run; it is also used as
// the job_id under which pod log lines are written, so the conductor relays
// them with the existing LIST_BUILD_LOGS cursor (no new log transport).
type RunOneShotJobRequest struct {
	RunID           string   `json:"runId"`                     // unique id for this run (also the log job_id)
	OSID            string   `json:"osid"`                      // app id
	Namespace       string   `json:"namespace"`                 // app namespace; defaults to OSID when empty
	Image           string   `json:"image"`                     // fully-resolved image ref (registry/runos-apps/<osid>:<sha>)
	Command         []string `json:"command"`                   // argv to run; replaces the image entrypoint (script or bare command)
	EnvConfigMap    string   `json:"envConfigMap,omitempty"`    // ConfigMap to inject via envFrom; defaults to <osid>-user-env-config
	EnvSecret       string   `json:"envSecret,omitempty"`       // Secret to inject via envFrom; defaults to <osid>-user-env-vars
	ImagePullSecret string   `json:"imagePullSecret,omitempty"` // Harbor pull secret name; omitted when the image is public
	TimeoutSeconds  int      `json:"timeoutSeconds,omitempty"`  // hard deadline; defaults to defaultOneShotTimeoutSeconds when <= 0
	Actor           string   `json:"actor,omitempty"`           // who triggered the run, for the audit record
}

// RunOneShotJobResponse acknowledges that the Job was created. The terminal
// status and exit code land on the audit record and are polled with
// ONESHOT_JOB_STATUS; pod logs land in buildkit_logs (keyed by RunID) and are
// polled with LIST_BUILD_LOGS, mirroring the VCS build flow.
type RunOneShotJobResponse struct {
	Success    bool   `json:"success"`
	RunID      string `json:"runId,omitempty"`
	K8sJobName string `json:"k8sJobName,omitempty"`
	Message    string `json:"message,omitempty"`
}

const (
	// defaultOneShotTimeoutSeconds bounds a run when the conductor sends no
	// timeout. Conductor normally supplies one (its AC10), but a missing
	// value must never produce an unbounded Job.
	defaultOneShotTimeoutSeconds = 1800 // 30m

	// oneShotTTLAfterFinishedSeconds is a cleanup backstop: even if the
	// conductor never sends CLEANUP_ONESHOT_JOB (e.g. it crashed), the
	// finished Job is garbage-collected by Kubernetes after this delay.
	// Explicit cleanup usually removes it sooner. Logs survive in SQLite.
	oneShotTTLAfterFinishedSeconds int32 = 600

	// oneShotTimeoutExitCode is the synthetic exit code recorded when a run
	// is killed by its deadline. 124 mirrors coreutils `timeout`.
	oneShotTimeoutExitCode = 124
)

// RunOneShotJob handles the RUN_ONESHOT_JOB instruction. It creates a one-shot
// Kubernetes Job from the app's prebuilt image in the app namespace, then
// returns immediately; an async tracker streams pod logs, captures the real
// exit code, and records the terminal outcome. A Job creates its own pod, so
// this works even when the app has no running pod (first deploy, crash-loop).
func RunOneShotJob(jsonB64 string) (string, string, error) {
	const replyType = "RUN_ONESHOT_JOB_RESPONSE"

	var req RunOneShotJobRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		log.Printf("RUN_ONESHOT_JOB: decode error: %v", err)
		return "", "", err
	}

	if req.RunID == "" || req.OSID == "" || req.Image == "" || len(req.Command) == 0 {
		return replyOneShot(replyType, RunOneShotJobResponse{
			Success: false,
			Message: "runId, osid, image, and a non-empty command are required",
		})
	}

	namespace := req.Namespace
	if namespace == "" {
		namespace = req.OSID
	}
	envConfigMap := req.EnvConfigMap
	if envConfigMap == "" {
		envConfigMap = fmt.Sprintf("%s-user-env-config", req.OSID)
	}
	envSecret := req.EnvSecret
	if envSecret == "" {
		envSecret = fmt.Sprintf("%s-user-env-vars", req.OSID)
	}
	timeoutSeconds := req.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultOneShotTimeoutSeconds
	}

	clientset, err := newClientset()
	if err != nil {
		log.Printf("RUN_ONESHOT_JOB: k8s client: %v", err)
		return replyOneShot(replyType, RunOneShotJobResponse{
			Success: false,
			Message: fmt.Sprintf("failed to create kubernetes client: %v", err),
		})
	}

	jobName := oneShotJobName(req.RunID)
	job := buildOneShotJob(jobName, namespace, req, envConfigMap, envSecret, timeoutSeconds)

	ctx := context.Background()
	if _, err := clientset.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		log.Printf("RUN_ONESHOT_JOB: create job %s/%s: %v", namespace, jobName, err)
		return replyOneShot(replyType, RunOneShotJobResponse{
			Success: false,
			Message: fmt.Sprintf("failed to create job: %v", err),
		})
	}

	// Persist the audit record after the Job is accepted by the API server.
	commandJSON, _ := json.Marshal(req.Command)
	if err := datastore.CreateOneShotJob(req.RunID, req.OSID, namespace, req.Image, string(commandJSON), jobName, req.Actor, timeoutSeconds); err != nil {
		// Non-fatal: the Job is already running. Log and continue so the run
		// still completes; the tracker's status updates will fail to find the
		// row, but logs and the K8s outcome are unaffected.
		log.Printf("RUN_ONESHOT_JOB: persist audit record for %s: %v", req.RunID, err)
	}

	datastore.InsertBuildKitLog(req.RunID, fmt.Sprintf("Created one-shot Job %s in namespace %s from image %s", jobName, namespace, req.Image))
	log.Printf("RUN_ONESHOT_JOB: created job %s/%s run_id=%s image=%s timeout=%ds", namespace, jobName, req.RunID, req.Image, timeoutSeconds)

	// Track to completion asynchronously so the gRPC call returns promptly,
	// matching the VCS_BUILD pattern.
	go trackOneShotJob(clientset, namespace, jobName, req.RunID, timeoutSeconds)

	return replyOneShot(replyType, RunOneShotJobResponse{
		Success:    true,
		RunID:      req.RunID,
		K8sJobName: jobName,
	})
}

// oneShotJobName derives a deterministic, DNS-safe Job name from the run id.
func oneShotJobName(runID string) string {
	name := fmt.Sprintf("runos-run-%s", sanitizeLabelValue(runID))
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// buildOneShotJob assembles the batch/v1 Job: a single pod that runs the
// command with the app's env injected, never restarts, and is bounded by a
// deadline.
func buildOneShotJob(jobName, namespace string, req RunOneShotJobRequest, envConfigMap, envSecret string, timeoutSeconds int) *batchv1.Job {
	backoffLimit := int32(0)
	activeDeadline := int64(timeoutSeconds)
	ttl := oneShotTTLAfterFinishedSeconds

	envFrom := []corev1.EnvFromSource{
		{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: envConfigMap}}},
		{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: envSecret}}},
	}

	var imagePullSecrets []corev1.LocalObjectReference
	if req.ImagePullSecret != "" {
		imagePullSecrets = []corev1.LocalObjectReference{{Name: req.ImagePullSecret}}
	}

	labels := map[string]string{
		"app.kubernetes.io/managed-by": "runos",
		"runos.io/oneshot-run-id":      sanitizeLabelValue(req.RunID),
		"runos.io/app":                 sanitizeLabelValue(req.OSID),
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			ActiveDeadlineSeconds:   &activeDeadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:    corev1.RestartPolicyNever,
					ImagePullSecrets: imagePullSecrets,
					Containers: []corev1.Container{
						{
							Name:    "runos-run",
							Image:   req.Image,
							Command: req.Command,
							EnvFrom: envFrom,
						},
					},
				},
			},
		},
	}
}

// trackOneShotJob waits for the Job's pod, streams its logs into the datastore,
// captures the real exit code, and records the terminal outcome. It owns no
// deletion: the conductor signals CLEANUP_ONESHOT_JOB, and TTLSecondsAfterFinished
// is the backstop. A context deadline a little beyond the Job's own deadline
// guards against the tracker hanging if the API never reports a terminal state.
func trackOneShotJob(clientset *k8s.Clientset, namespace, jobName, runID string, timeoutSeconds int) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds+120)*time.Second)
	defer cancel()

	datastore.UpdateOneShotJobStatus(runID, datastore.OneShotStatusRunning)

	podName, err := waitForOneShotPod(ctx, clientset, namespace, jobName, runID)
	if err != nil {
		msg := fmt.Sprintf("ERROR: one-shot pod never started: %v", err)
		datastore.InsertBuildKitLog(runID, msg)
		log.Printf("RUN_ONESHOT_JOB: %s (run_id=%s)", msg, runID)
		finalizeOneShot(ctx, clientset, namespace, jobName, podName, runID)
		return
	}

	streamOneShotLogs(ctx, clientset, namespace, podName, runID)
	finalizeOneShot(ctx, clientset, namespace, jobName, podName, runID)
}

// waitForOneShotPod polls for the pod the Job controller creates and returns
// its name once it has started (or terminated). Waiting reasons such as
// ImagePullBackOff are surfaced to the run log so a stuck pull is visible.
func waitForOneShotPod(ctx context.Context, clientset *k8s.Clientset, namespace, jobName, runID string) (string, error) {
	selector := fmt.Sprintf("job-name=%s", jobName)
	var lastReason string
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for pod")
		default:
		}

		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err == nil && len(pods.Items) > 0 {
			pod := pods.Items[0]
			// Surface a blocking waiting reason (image pull, etc.) once.
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != lastReason {
					lastReason = cs.State.Waiting.Reason
					datastore.InsertBuildKitLog(runID, fmt.Sprintf("pod %s: %s: %s", pod.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message))
				}
			}
			// Ready to stream once the container has run or terminated.
			if pod.Status.Phase != corev1.PodPending {
				return pod.Name, nil
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Running != nil || cs.State.Terminated != nil {
					return pod.Name, nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for pod")
		case <-time.After(2 * time.Second):
		}
	}
}

// streamOneShotLogs follows the pod's stdout/stderr and writes each line into
// buildkit_logs under runID. It returns when the container terminates (the
// stream closes) or the context is cancelled.
func streamOneShotLogs(ctx context.Context, clientset *k8s.Clientset, namespace, podName, runID string) {
	stream, err := clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{Follow: true}).Stream(ctx)
	if err != nil {
		// A sub-second container can exit and be torn down before we attach
		// (the API then reports the container "is not available"). That is not
		// a run failure: the terminal status + exit code are resolved
		// separately from the pod/Job terminated state, so a missing log stream
		// only costs us the logs. Surface a benign note rather than an ERROR
		// that reads like the cause of a failure.
		datastore.InsertBuildKitLog(runID, "(no logs captured; container finished too quickly to attach a log stream)")
		log.Printf("RUN_ONESHOT_JOB: log stream attach skipped for run_id=%s: %v", runID, err)
		return
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		datastore.InsertBuildKitLog(runID, scanner.Text())
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		datastore.InsertBuildKitLog(runID, fmt.Sprintf("WARNING: log stream ended with error: %v", err))
	}
}

// finalizeOneShot resolves the terminal status + exit code and records it.
// A deadline kill is reported as timeout/124; otherwise the container's real
// terminated exit code decides success (0) vs failed (non-zero).
func finalizeOneShot(ctx context.Context, clientset *k8s.Clientset, namespace, jobName, podName, runID string) {
	status, exitCode := resolveOneShotOutcome(ctx, clientset, namespace, jobName, podName)
	if err := datastore.UpdateOneShotJobResult(runID, status, exitCode); err != nil {
		log.Printf("RUN_ONESHOT_JOB: record result for %s: %v", runID, err)
	}
	datastore.InsertBuildKitLog(runID, fmt.Sprintf("Run finished: status=%s exitCode=%d", status, exitCode))
	log.Printf("RUN_ONESHOT_JOB: run_id=%s terminal status=%s exit=%d", runID, status, exitCode)
}

// oneShotOutcomeInputs is the snapshot of Job + pod state that decides a run's
// terminal classification. Keeping it a plain struct lets the decision logic
// (classifyOneShotOutcome) be unit-tested without a live cluster.
type oneShotOutcomeInputs struct {
	deadlineExceeded bool // Job has a Failed condition with reason DeadlineExceeded
	jobSucceeded     bool // Job.Status.Succeeded > 0
	jobFailed        bool // Job.Status.Failed > 0
	podSucceeded     bool // Pod.Status.Phase == Succeeded (aggregate: all containers exited 0)
	podFailed        bool // Pod.Status.Phase == Failed (aggregate: a container exited non-zero)
	podTerminated    bool // a container terminated state was readable
	podExitCode      int  // that container's exit code (valid only when podTerminated)
}

// hasTerminalSignal reports whether any authoritative terminal evidence has
// landed yet: a per-container terminated state, an aggregate pod phase, a Job
// counter, or a deadline kill. resolveOneShotOutcome keeps polling while this
// is false rather than inventing an outcome, which is what previously turned a
// sub-second success into a false failure when the evidence had not yet
// surfaced. "Indeterminate" is only ever reached when the context expires
// (a genuine hang), where classifyOneShotOutcome's failed/1 fallback applies.
func (in oneShotOutcomeInputs) hasTerminalSignal() bool {
	return in.deadlineExceeded || in.podTerminated ||
		in.podSucceeded || in.podFailed || in.jobSucceeded || in.jobFailed
}

// oneShotOutcomeWindow bounds how long resolveOneShotOutcome waits, after a
// signal-coded container exit (137/143), for the Job controller to write a
// DeadlineExceeded condition before trusting the raw signal exit code. A
// deadline condition lands within a second or two in practice; the window only
// adds latency to the rare case of a genuine signal-coded self-exit that is not
// a deadline kill. It does NOT bound the wait for a not-yet-terminal run: that
// waits for a real terminal signal up to the context deadline (see
// resolveOneShotOutcome), so a slow-to-surface success is never failed early.
const (
	oneShotOutcomeWindow   = 10 * time.Second
	oneShotOutcomeInterval = 1 * time.Second
)

// classifyOneShotOutcome maps a state snapshot to (status, exitCode). A deadline
// kill is authoritative (timeout/124). Otherwise the container's real terminated
// exit code decides success(0) vs failed(code); when the container state is
// unavailable the Job's counters are the fallback, and a wholly indeterminate
// state is treated as failed/1 so a non-zero result always propagates rather
// than a false success.
func classifyOneShotOutcome(in oneShotOutcomeInputs) (string, int) {
	if in.deadlineExceeded {
		return datastore.OneShotStatusTimeout, oneShotTimeoutExitCode
	}
	// The per-container terminated state carries the real exit code, so it
	// wins over the aggregate phase/counters when readable.
	if in.podTerminated {
		if in.podExitCode == 0 {
			return datastore.OneShotStatusSuccess, 0
		}
		return datastore.OneShotStatusFailed, in.podExitCode
	}
	// Aggregate terminal signals (pod phase or Job counters) resolve a fast run
	// whose per-container exit code was torn down before we could read it. A
	// Succeeded phase / counter means every container exited 0.
	if in.jobSucceeded || in.podSucceeded {
		return datastore.OneShotStatusSuccess, 0
	}
	if in.jobFailed || in.podFailed {
		return datastore.OneShotStatusFailed, 1
	}
	// Wholly indeterminate: never a false success.
	return datastore.OneShotStatusFailed, 1
}

// looksLikeSignalKill reports whether an exit code is the 128+signal shape a
// container gets when killed (137=SIGKILL, 143=SIGTERM) — the codes a deadline
// kill produces. A clean code like 0 or 7 is not a signal kill, so it can be
// classified immediately with no added latency.
func looksLikeSignalKill(exitCode int) bool {
	return exitCode >= 128
}

// readOutcomeInputs snapshots the current Job + pod state.
func readOutcomeInputs(ctx context.Context, clientset *k8s.Clientset, namespace, jobName, podName string) oneShotOutcomeInputs {
	var in oneShotOutcomeInputs
	if job, err := clientset.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{}); err == nil {
		in.jobSucceeded = job.Status.Succeeded > 0
		in.jobFailed = job.Status.Failed > 0
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue && c.Reason == "DeadlineExceeded" {
				in.deadlineExceeded = true
			}
		}
	}
	if podName != "" {
		if pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{}); err == nil {
			// Aggregate phase is often readable a beat before the kubelet patches
			// the per-container terminated state back onto the pod, so it is the
			// earlier reliable signal for a fast run.
			switch pod.Status.Phase {
			case corev1.PodSucceeded:
				in.podSucceeded = true
			case corev1.PodFailed:
				in.podFailed = true
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Terminated != nil {
					in.podTerminated = true
					in.podExitCode = int(cs.State.Terminated.ExitCode)
					break
				}
			}
		}
	}
	return in
}

// resolveOneShotOutcome decides the terminal status + exit code. A clean
// (non-signal) container exit is classified immediately so a normal non-zero
// command (e.g. exit 7) propagates its real code with zero added latency. When
// the pod was signal-killed (or no terminal state is visible yet) the Job is
// polled for a short window so a deadline kill is reliably reported as
// timeout/124 instead of racing to the pod's 137 (SIGKILL) before the Job
// controller writes its DeadlineExceeded condition.
func resolveOneShotOutcome(ctx context.Context, clientset *k8s.Clientset, namespace, jobName, podName string) (string, int) {
	start := time.Now()
	for {
		in := readOutcomeInputs(ctx, clientset, namespace, jobName, podName)

		// A confirmed deadline kill is authoritative.
		if in.deadlineExceeded {
			return classifyOneShotOutcome(in)
		}
		// An unambiguous clean container exit needs no waiting.
		if in.podTerminated && !looksLikeSignalKill(in.podExitCode) {
			return classifyOneShotOutcome(in)
		}
		// A signal-coded container exit may be a deadline kill whose
		// DeadlineExceeded condition has not landed yet. Wait a bounded window
		// for it before trusting the raw 137/143.
		if in.podTerminated && looksLikeSignalKill(in.podExitCode) {
			if time.Since(start) >= oneShotOutcomeWindow {
				return classifyOneShotOutcome(in)
			}
			select {
			case <-ctx.Done():
				return classifyOneShotOutcome(in)
			case <-time.After(oneShotOutcomeInterval):
			}
			continue
		}
		// No per-container terminated state is readable. A sub-second container
		// can exit and be torn down before its exit code is patched back onto
		// the pod; the aggregate pod phase / Job counters surface the outcome a
		// beat later. Resolve as soon as any such terminal signal lands rather
		// than giving up to a false failure after a fixed window. A real
		// completion always surfaces a terminal signal within seconds; only a
		// genuine hang keeps this false until the context deadline, where the
		// indeterminate fallback (failed/1) then correctly applies.
		if in.hasTerminalSignal() {
			return classifyOneShotOutcome(in)
		}
		select {
		case <-ctx.Done():
			return classifyOneShotOutcome(in)
		case <-time.After(oneShotOutcomeInterval):
		}
	}
}

// sanitizeLabelValue maps a value to the DNS-1123-ish subset Kubernetes labels
// and names accept: lowercase alphanumerics and '-'. Other runes become '-'.
func sanitizeLabelValue(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '-')
		}
	}
	if len(out) > 63 {
		out = out[:63]
	}
	return string(out)
}

// newClientset builds an in-cluster Kubernetes clientset. The instructions
// package cannot import agentstream (that package imports instructions), so
// the client is constructed here directly, mirroring put_secret_file.go.
func newClientset() (*k8s.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	return k8s.NewForConfig(config)
}

func replyOneShot(replyType string, payload RunOneShotJobResponse) (string, string, error) {
	encoded, err := commons.JsonB64Encode(payload)
	if err != nil {
		return "", "", err
	}
	return replyType, encoded, nil
}
