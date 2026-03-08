// Package k8s provides Kubernetes API integration for mapping GPU devices
// to the pods and workloads that are using them.
package k8s

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// GPUResourceNames are the Kubernetes resource names used by GPU device plugins.
var GPUResourceNames = []string{
	"nvidia.com/gpu",
	"amd.com/gpu",
	"gpu.intel.com/i915",
	"gpu.intel.com/xe",
}

// PodGPUInfo holds information about a pod that has GPU resources assigned.
type PodGPUInfo struct {
	PodName       string
	Namespace     string
	ContainerName string
	NodeName      string
	// GPUCount is the number of GPUs requested by this pod.
	GPUCount int64
	// Vendor is derived from the resource name.
	Vendor string
}

// Mapper watches the Kubernetes API and maintains a mapping of
// node → GPU vendor → list of pods using GPUs on that node.
type Mapper struct {
	log    *zap.Logger
	client kubernetes.Interface

	mu     sync.RWMutex
	byNode map[string][]PodGPUInfo // node name → pods with GPUs
}

// NewMapper creates a Mapper using in-cluster config or the provided kubeconfig path.
func NewMapper(log *zap.Logger, kubeconfig string) (*Mapper, error) {
	var cfg *rest.Config
	var err error

	if kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("in-cluster config: %w", err)
		}
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig: %w", err)
		}
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}

	return &Mapper{
		log:    log,
		client: client,
		byNode: make(map[string][]PodGPUInfo),
	}, nil
}

// Start begins a background refresh loop that re-syncs pod-GPU mappings
// every refreshInterval. It stops when ctx is cancelled.
func (m *Mapper) Start(ctx context.Context, refreshInterval time.Duration) {
	m.refresh(ctx)
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refresh(ctx)
		}
	}
}

// refresh lists all pods across all namespaces and rebuilds the node→pods map.
func (m *Mapper) refresh(ctx context.Context) {
	pods, err := m.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		m.log.Warn("failed to list pods", zap.Error(err))
		return
	}

	byNode := make(map[string][]PodGPUInfo)
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName == "" {
			continue
		}
		for _, container := range pod.Spec.Containers {
			for _, resName := range GPUResourceNames {
				qty, ok := container.Resources.Limits[corev1.ResourceName(resName)]
				if !ok {
					continue
				}
				count, _ := qty.AsInt64()
				if count <= 0 {
					continue
				}
				byNode[pod.Spec.NodeName] = append(byNode[pod.Spec.NodeName], PodGPUInfo{
					PodName:       pod.Name,
					Namespace:     pod.Namespace,
					ContainerName: container.Name,
					NodeName:      pod.Spec.NodeName,
					GPUCount:      count,
					Vendor:        vendorFromResource(resName),
				})
			}
		}
	}

	m.mu.Lock()
	m.byNode = byNode
	m.mu.Unlock()
}

// PodsOnNode returns all pods with GPU resources on the given node.
func (m *Mapper) PodsOnNode(nodeName string) []PodGPUInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byNode[nodeName]
}

// vendorFromResource maps a Kubernetes resource name to a vendor string.
func vendorFromResource(resName string) string {
	switch {
	case strings.HasPrefix(resName, "nvidia"):
		return "nvidia"
	case strings.HasPrefix(resName, "amd"):
		return "amd"
	case strings.HasPrefix(resName, "gpu.intel"):
		return "intel"
	default:
		return "unknown"
	}
}
