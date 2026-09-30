package instructions

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// OneShotResources is the optional resource block of RUN_ONESHOT_JOB. The
// conductor fills it from the cluster's one-shot settings. A zero or negative
// field means "not set". Unknown JSON fields are ignored on decode, so a newer
// conductor can add fields without breaking this agent.
type OneShotResources struct {
	CPURequestMc              int `json:"cpuRequestMc,omitempty"`              // CPU request, millicores
	CPULimitMc                int `json:"cpuLimitMc,omitempty"`                // CPU limit, millicores
	MemoryRequestMb           int `json:"memoryRequestMb,omitempty"`           // memory request, MiB
	MemoryLimitMb             int `json:"memoryLimitMb,omitempty"`             // memory limit, MiB
	EphemeralStorageRequestMb int `json:"ephemeralStorageRequestMb,omitempty"` // ephemeral-storage request, MiB
	EphemeralStorageLimitMb   int `json:"ephemeralStorageLimitMb,omitempty"`   // ephemeral-storage limit, MiB
}

// Defaults used for any request the conductor did not send (an older conductor
// sends no block at all). They are requests only, never limits: the pod stops
// being BestEffort, so it is no longer the first eviction target, and a run that
// passed before cannot start to fail on a new CPU, memory or disk cap.
const (
	defaultOneShotCPURequestMc              = 100
	defaultOneShotMemoryRequestMb           = 256
	defaultOneShotEphemeralStorageRequestMb = 1024
)

// oneShotResourceRequirements turns the optional block into container
// requirements. The result always has a request for CPU, memory and
// ephemeral-storage. A request above its limit is lowered to the limit,
// because the Kubernetes API would refuse the whole Job otherwise. A limit
// without a storage request makes the request equal to the limit, so the
// scheduler places the pod only where the space exists.
func oneShotResourceRequirements(r *OneShotResources) corev1.ResourceRequirements {
	if r == nil {
		r = &OneShotResources{}
	}
	requests := corev1.ResourceList{}
	limits := corev1.ResourceList{}

	set := func(name corev1.ResourceName, request, limit, defRequest int, mk func(int) resource.Quantity) {
		if request <= 0 {
			request = defRequest
			if limit > 0 && name == corev1.ResourceEphemeralStorage {
				request = limit
			}
		}
		if limit > 0 {
			if request > limit {
				request = limit
			}
			limits[name] = mk(limit)
		}
		requests[name] = mk(request)
	}
	milli := func(v int) resource.Quantity { return *resource.NewMilliQuantity(int64(v), resource.DecimalSI) }
	mib := func(v int) resource.Quantity { return *resource.NewQuantity(int64(v)*1024*1024, resource.BinarySI) }

	set(corev1.ResourceCPU, r.CPURequestMc, r.CPULimitMc, defaultOneShotCPURequestMc, milli)
	set(corev1.ResourceMemory, r.MemoryRequestMb, r.MemoryLimitMb, defaultOneShotMemoryRequestMb, mib)
	set(corev1.ResourceEphemeralStorage, r.EphemeralStorageRequestMb, r.EphemeralStorageLimitMb, defaultOneShotEphemeralStorageRequestMb, mib)

	out := corev1.ResourceRequirements{Requests: requests}
	if len(limits) > 0 {
		out.Limits = limits
	}
	return out
}
