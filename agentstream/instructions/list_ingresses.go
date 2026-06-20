package instructions

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/runos-official/clusteragent/commons"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	k8sClientset     *k8s.Clientset
	k8sClientsetOnce sync.Once
	k8sClientsetErr  error
)

func getK8sClientset() (*k8s.Clientset, error) {
	k8sClientsetOnce.Do(func() {
		config, err := rest.InClusterConfig()
		if err != nil {
			k8sClientsetErr = fmt.Errorf("failed to get in-cluster config: %w", err)
			return
		}
		k8sClientset, k8sClientsetErr = k8s.NewForConfig(config)
	})
	return k8sClientset, k8sClientsetErr
}

type IngressEntry struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Host        string `json:"host"`
	ServiceName string `json:"service_name"`
	ServicePort int32  `json:"service_port"`
}

type ListIngressesResponse struct {
	Ingresses []IngressEntry `json:"ingresses"`
}

func ListIngresses(jsonB64 string) (string, string, error) {
	clientset, err := getK8sClientset()
	if err != nil {
		log.Printf("Failed to get K8s clientset: %v", err)
		return "", "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ingressList, err := clientset.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{
		ResourceVersion: "0",
	})
	if err != nil {
		log.Printf("Failed to list ingresses: %v", err)
		return "", "", err
	}

	var entries []IngressEntry
	for _, ing := range ingressList.Items {
		if len(ing.Spec.Rules) == 0 {
			continue
		}
		rule := ing.Spec.Rules[0]
		if rule.HTTP == nil || len(rule.HTTP.Paths) == 0 {
			continue
		}
		path := rule.HTTP.Paths[0]
		if path.Backend.Service == nil {
			continue
		}

		entries = append(entries, IngressEntry{
			Name:        ing.Name,
			Namespace:   ing.Namespace,
			Host:        rule.Host,
			ServiceName: path.Backend.Service.Name,
			ServicePort: path.Backend.Service.Port.Number,
		})
	}

	if entries == nil {
		entries = []IngressEntry{}
	}

	resp := ListIngressesResponse{Ingresses: entries}
	respB64, err := commons.JsonB64Encode(resp)
	if err != nil {
		return "", "", err
	}

	return "LIST_INGRESSES_RESPONSE", respB64, nil
}
