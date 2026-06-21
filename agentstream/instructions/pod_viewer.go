package instructions

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/runos-official/clusteragent/commons"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"k8s.io/apimachinery/pkg/api/resource"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

const (
	// podListTimeout bounds the pod + metrics list calls so a slow/unreachable
	// API server can't hang the handler indefinitely.
	podListTimeout = 30 * time.Second
	// podListServerCap bounds how many pod objects we pull from the API server
	// in one List. Filtering by searchTerm happens client-side, so this is a
	// memory safety cap (a huge cluster shouldn't be paged entirely into the
	// agent), set well above the per-request display Limit.
	podListServerCap = 2000
)

type k8sRequest struct {
	SearchTerm string `json:"searchTerm"`
	Limit      int    `json:"limit"`
}

type k8sPodResponse struct {
	Name          string `json:"name"`
	Namespace     string `json:"namespace"`
	Status        string `json:"status"`
	Restarts      int32  `json:"restarts"`
	Age           string `json:"age"`
	Node          string `json:"node"`
	CPUUsageMc    int64  `json:"cpuUsageMc"`    // in millicores
	MemoryUsageMb int64  `json:"memoryUsageMb"` // in MB
}

type k8sResponse struct {
	Pods  []k8sPodResponse `json:"pods"`
	Total int              `json:"total"`
}

func K8sPodStatsRequestFromServer(jsonB64 string) (string, string, error) {
	var req k8sRequest
	if err := commons.JsonB64Decode(jsonB64, &req); err != nil {
		log.Printf("Error decoding message: %s", err)
		return "", "", err
	}
	if req.Limit <= 0 {
		req.Limit = 50
	}
	// Log metadata only (never the raw payload), for parity with the other handlers.
	log.Printf("K8sPodStatsRequestFromServer: searchTerm=%q limit=%d", req.SearchTerm, req.Limit)

	// In-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("Failed to get in-cluster config: %v", err)
		return "", "", err
	}

	clientset, err := k8s.NewForConfig(config)
	if err != nil {
		log.Printf("Failed to create clientset: %v", err)
		return "", "", err
	}

	metricsClient, err := metricsclient.NewForConfig(config)
	if err != nil {
		log.Printf("Failed to create metrics client: %v", err)
		return "", "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), podListTimeout)
	defer cancel()

	// List pods, bounded server-side so a huge cluster can't be paged entirely
	// into the agent. Filtering by searchTerm is applied client-side below.
	pods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{Limit: podListServerCap})
	if err != nil {
		log.Printf("Failed to list pods: %v", err)
		return "", "", err
	}

	// Get metrics for all pods
	podMetricsList, err := metricsClient.MetricsV1beta1().PodMetricses("").List(ctx, metav1.ListOptions{Limit: podListServerCap})
	if err != nil {
		log.Printf("Failed to list pod metrics: %v", err)
	}

	// Map pod metrics by namespace/name
	metricsMap := make(map[string]map[string]struct {
		CPU    resource.Quantity
		Memory resource.Quantity
	})
	if podMetricsList != nil {
		for _, m := range podMetricsList.Items {
			totalCPU := resource.NewQuantity(0, resource.DecimalSI)
			totalMem := resource.NewQuantity(0, resource.BinarySI)
			for _, c := range m.Containers {
				totalCPU.Add(c.Usage["cpu"])
				totalMem.Add(c.Usage["memory"])
			}
			if metricsMap[m.Namespace] == nil {
				metricsMap[m.Namespace] = make(map[string]struct {
					CPU    resource.Quantity
					Memory resource.Quantity
				})
			}
			metricsMap[m.Namespace][m.Name] = struct {
				CPU    resource.Quantity
				Memory resource.Quantity
			}{
				CPU:    *totalCPU,
				Memory: *totalMem,
			}
		}
	}

	var results []k8sPodResponse
	count := 0

	for _, pod := range pods.Items {
		if count >= req.Limit {
			break
		}

		if req.SearchTerm != "" &&
			!strings.Contains(strings.ToLower(pod.Name), strings.ToLower(req.SearchTerm)) &&
			!strings.Contains(strings.ToLower(pod.Namespace), strings.ToLower(req.SearchTerm)) {
			continue
		}

		restarts := int32(0)
		for _, cs := range pod.Status.ContainerStatuses {
			restarts += cs.RestartCount
		}

		age := humanReadableAge(pod.CreationTimestamp.Time)

		var cpuMc, memMb int64
		if nsMetrics, ok := metricsMap[pod.Namespace]; ok {
			if pm, ok := nsMetrics[pod.Name]; ok {
				cpuMc = pm.CPU.MilliValue()
				memMb = pm.Memory.Value() / (1024 * 1024)
			}
		}

		results = append(results, k8sPodResponse{
			Name:          pod.Name,
			Namespace:     pod.Namespace,
			Status:        getPodStatus(&pod),
			Restarts:      restarts,
			Age:           age,
			Node:          pod.Spec.NodeName,
			CPUUsageMc:    cpuMc,
			MemoryUsageMb: memMb,
		})
		count++
	}

	final := k8sResponse{
		Pods:  results,
		Total: len(results),
	}

	jsonResponse, err := commons.JsonB64Encode(final)
	if err != nil {
		log.Printf("Error encoding response: %v", err)
		return "", "", err
	}

	return "K8S_POD_STATS_RESPONSE", jsonResponse, nil
}

func getPodStatus(pod *corev1.Pod) string {
	// Check if terminating
	if pod.DeletionTimestamp != nil {
		return "Terminating"
	}

	// If no container statuses, fall back to phase
	if len(pod.Status.ContainerStatuses) == 0 {
		return string(pod.Status.Phase)
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			return cs.State.Terminated.Reason
		}
	}

	// Fallback: return phase
	return string(pod.Status.Phase)
}

func humanReadableAge(t time.Time) string {
	d := time.Since(t)
	if d.Hours() > 24 {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	} else if d.Hours() >= 1 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	} else if d.Minutes() >= 1 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
