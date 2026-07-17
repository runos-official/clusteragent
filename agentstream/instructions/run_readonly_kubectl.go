package instructions

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"time"

	"github.com/runos-official/clusteragent/commons"

	corev1 "k8s.io/api/core/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// RunReadonlyKubectlRequest carries an arbitrary kubectl argv (WITHOUT the
// leading "kubectl") to run against the cluster as the read-only assistant SA.
// The LLM is intentionally not restricted: the reader-exec pod's read-only RBAC
// identity is the guardrail, so writes are denied by Kubernetes and there is no
// stronger credential in that pod to steal.
type RunReadonlyKubectlRequest struct {
	Args           []string `json:"args"`
	TimeoutSeconds int      `json:"timeoutSeconds,omitempty"`
}

type RunReadonlyKubectlResponse struct {
	Success  bool   `json:"success"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
	Message  string `json:"message,omitempty"`
}

const (
	defaultReadonlyKubectlTimeout = 30 // seconds
	maxReadonlyKubectlTimeout     = 120
)

// RunReadonlyKubectl handles RUN_READONLY_KUBECTL: ensure the shared reader-exec
// pod is up, then exec ["kubectl", ...args] inside it and return stdout/stderr/
// exit code. A non-zero kubectl exit (e.g. "not found") is a normal result, not
// a transport error.
func RunReadonlyKubectl(jsonB64 string) (string, string, error) {
	const replyType = "RUN_READONLY_KUBECTL_RESPONSE"

	var req RunReadonlyKubectlRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		log.Printf("RUN_READONLY_KUBECTL: decode error: %v", err)
		return "", "", err
	}
	if len(req.Args) == 0 {
		return replyReadonlyKubectl(replyType, RunReadonlyKubectlResponse{
			Success: false, Message: "args must be a non-empty kubectl argv",
		})
	}

	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultReadonlyKubectlTimeout
	}
	if timeout > maxReadonlyKubectlTimeout {
		timeout = maxReadonlyKubectlTimeout
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		return replyReadonlyKubectl(replyType, RunReadonlyKubectlResponse{
			Success: false, Message: fmt.Sprintf("in-cluster config: %v", err),
		})
	}
	clientset, err := newClientset()
	if err != nil {
		return replyReadonlyKubectl(replyType, RunReadonlyKubectlResponse{
			Success: false, Message: fmt.Sprintf("kubernetes client: %v", err),
		})
	}

	// A little headroom over the exec deadline for pod spin-up + streaming.
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout+int(readerReadyTimeout/time.Second)+10)*time.Second)
	defer cancel()

	if err := ensureReaderExecPod(ctx, clientset); err != nil {
		log.Printf("RUN_READONLY_KUBECTL: ensure reader pod: %v", err)
		return replyReadonlyKubectl(replyType, RunReadonlyKubectlResponse{
			Success: false, Message: fmt.Sprintf("reader-exec pod not ready: %v", err),
		})
	}

	execCtx, execCancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer execCancel()

	stdout, stderr, exitCode, err := execInReaderPod(execCtx, config, clientset, req.Args)
	if err != nil {
		// Transport / exec-setup failure (not a kubectl non-zero exit).
		log.Printf("RUN_READONLY_KUBECTL: exec error: %v", err)
		return replyReadonlyKubectl(replyType, RunReadonlyKubectlResponse{
			Success: false, Stdout: stdout, Stderr: stderr, ExitCode: exitCode,
			Message: fmt.Sprintf("exec failed: %v", err),
		})
	}

	return replyReadonlyKubectl(replyType, RunReadonlyKubectlResponse{
		Success: exitCode == 0, Stdout: stdout, Stderr: stderr, ExitCode: exitCode,
	})
}

// execInReaderPod runs ["kubectl", ...args] in the reader-exec pod. Returns the
// captured stdout/stderr and the process exit code. A non-zero kubectl exit is
// returned as (…, exitCode, nil); only a real transport/setup failure returns a
// non-nil error.
func execInReaderPod(ctx context.Context, config *rest.Config, clientset *k8s.Clientset, args []string) (string, string, int, error) {
	command := append([]string{"kubectl"}, args...)

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(readerPod).Namespace(Namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: readerContainer,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return "", "", 0, fmt.Errorf("build executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		// A non-zero kubectl exit surfaces as a CodeExitError; treat it as a
		// normal (non-error) result carrying the exit code.
		if ee, ok := err.(utilexec.ExitError); ok && ee.Exited() {
			return stdout.String(), stderr.String(), ee.ExitStatus(), nil
		}
		return stdout.String(), stderr.String(), 0, err
	}
	return stdout.String(), stderr.String(), 0, nil
}

func replyReadonlyKubectl(replyType string, payload RunReadonlyKubectlResponse) (string, string, error) {
	encoded, err := commons.JsonB64Encode(payload)
	if err != nil {
		return "", "", err
	}
	return replyType, encoded, nil
}
