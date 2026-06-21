package agentstream

import (
	"testing"

	"github.com/google/uuid"
	"github.com/runos-official/clusteragent/agentstream/instructions"
	"github.com/runos-official/clusteragent/agentstream/l2sec"
)

// TestSafeHandleInstructionRecoversFromPanic pins the panic boundary: a handler
// that panics must NOT unwind the goroutine, because the cluster agent is a
// single per-cluster pod with no worker-pool isolation, so one unrecovered panic
// would CrashLoopBackOff the whole control surface. safeHandleInstruction must
// recover and let the stream loop keep serving. Without the recover, the panic
// propagates out of safeHandleInstruction and fails this test.
func TestSafeHandleInstructionRecoversFromPanic(t *testing.T) {
	const typ = "TEST_PANIC_HANDLER_DO_NOT_USE"
	instructions.Map[typ] = func(string) (string, string, error) {
		panic("boom from a handler")
	}
	t.Cleanup(func() { delete(instructions.Map, typ) })

	inst := &l2sec.ToClusterAgent{
		Tag:  uuid.New().String(),
		Type: typ,
	}

	// If safeHandleInstruction did not recover, these panic and fail the test.
	safeHandleInstruction(inst) // handler panic
	safeHandleInstruction(nil)  // nil instruction: the recovery path itself must not re-panic
}
