package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/containerd-shim-applevm-v2/config"
	"github.com/containerd-shim-applevm-v2/pkg/hypervisor"
	"github.com/containerd-shim-applevm-v2/pkg/provider"
	"github.com/containerd-shim-applevm-v2/pkg/server"
)

var version = "dev"

const (
	defaultNodeName = "applevm-mac"
	taintKey        = "virtual-kubelet.io/provider"
	taintValue      = "applevm"
	heartbeatSec    = 10
	kubeletPort     = 10250
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	nodeName := os.Getenv("APPLEVM_NODE_NAME")
	if nodeName == "" {
		hostname, _ := os.Hostname()
		if hostname != "" {
			nodeName = "applevm-" + sanitizeNodeName(hostname)
		} else {
			nodeName = defaultNodeName
		}
	}

	log.G(ctx).WithField("node", nodeName).WithField("version", version).Info("Starting Apple VM virtual kubelet")

	clientset, err := buildKubeClient()
	if err != nil {
		log.G(ctx).WithError(err).Fatal("Failed to create Kubernetes client")
	}

	cfg, err := config.Load("")
	if err != nil {
		log.G(ctx).WithError(err).Fatal("Failed to load config")
	}

	p := provider.New(nodeName, cfg, clientset)
	hv := hypervisor.NewAppleVM()

	nodeIP := detectNodeIP()
	if nodeIP == "" {
		log.G(ctx).Warn("Could not detect routable node IP; kubectl logs will not work")
	} else {
		log.G(ctx).WithField("ip", nodeIP).Info("Detected node IP")
	}

	node := buildNode(nodeName, nodeIP)
	if err := registerNode(ctx, clientset, node); err != nil {
		log.G(ctx).WithError(err).Fatal("Failed to register node")
	}
	log.G(ctx).WithField("node", nodeName).Info("Node registered")

	ctrl := provider.NewController(clientset, p, nodeName)
	go ctrl.Run(ctx)
	go heartbeat(ctx, clientset, nodeName)

	kubeletSrv := server.New(hv, kubeletPort, nodeIP, p, cfg.TLS.ClientCAPath)
	go func() {
		if err := kubeletSrv.Run(ctx); err != nil {
			log.G(ctx).WithError(err).Fatal("Kubelet HTTPS server failed")
		}
	}()

	<-ctx.Done()
	log.G(ctx).Info("Shutting down")
}

func buildKubeClient() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			home, _ := os.UserHomeDir()
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("building kubeconfig: %w", err)
		}
	}

	retVal, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}
	return retVal, nil
}

func buildNode(nodeName string, nodeIP string) *corev1.Node {
	addresses := []corev1.NodeAddress{
		{Type: corev1.NodeHostName, Address: nodeName},
	}
	if nodeIP != "" {
		addresses = append(addresses, corev1.NodeAddress{
			Type: corev1.NodeInternalIP, Address: nodeIP,
		})
	}

	retVal := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Labels: map[string]string{
				"type":                       "virtual-kubelet",
				"kubernetes.io/role":         "agent",
				"kubernetes.io/os":           "darwin",
				"kubernetes.io/arch":         "arm64",
				"node.kubernetes.io/applevm": "true",
			},
		},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{
				{Key: taintKey, Value: taintValue, Effect: corev1.TaintEffectNoSchedule},
			},
		},
		Status: corev1.NodeStatus{
			Phase: corev1.NodeRunning,
			Conditions: []corev1.NodeCondition{
				{
					Type:               corev1.NodeReady,
					Status:             corev1.ConditionTrue,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Reason:             "KubeletReady",
					Message:            "Apple VM virtual kubelet is ready",
				},
			},
			Addresses: addresses,
			DaemonEndpoints: corev1.NodeDaemonEndpoints{
				KubeletEndpoint: corev1.DaemonEndpoint{Port: kubeletPort},
			},
			Capacity:    detectCapacity(),
			Allocatable: detectCapacity(),
			NodeInfo: corev1.NodeSystemInfo{
				Architecture:            "arm64",
				OperatingSystem:         "darwin",
				KubeletVersion:          "v1.33.0",
				ContainerRuntimeVersion: "applevm://0.1.0",
			},
		},
	}
	return retVal
}

func registerNode(ctx context.Context, clientset *kubernetes.Clientset, node *corev1.Node) error {
	_, err := clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err == nil {
		return nil
	}

	// Node exists — update it
	existing, getErr := clientset.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("creating node: %w; getting existing: %w", err, getErr)
	}

	// Only update fields we own — don't overwrite Spec.PodCIDRs etc.
	existing.Labels = node.Labels
	existing.Spec.Taints = node.Spec.Taints
	updated, updateErr := clientset.CoreV1().Nodes().Update(ctx, existing, metav1.UpdateOptions{})
	if updateErr != nil {
		return fmt.Errorf("updating node: %w", updateErr)
	}
	updated.Status = node.Status
	_, err = clientset.CoreV1().Nodes().UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	return err
}

// detectNodeIP finds the host's routable IP address by dialing a UDP socket.
// No actual traffic is sent — this just determines which local address the OS
// would use for outbound traffic.
func detectNodeIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()

	retVal := conn.LocalAddr().(*net.UDPAddr).IP.String()
	return retVal
}

func detectCapacity() corev1.ResourceList {
	cpus := runtime.NumCPU()

	memMiB := uint64(8 * 1024) // default 8Gi
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err == nil {
		var memBytes uint64
		if _, parseErr := fmt.Sscan(strings.TrimSpace(string(out)), &memBytes); parseErr == nil {
			memMiB = memBytes / (1024 * 1024)
		}
	}

	retVal := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", cpus)),
		corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dMi", memMiB)),
		corev1.ResourcePods:   resource.MustParse("100"),
	}
	return retVal
}

var nonAlphanumDash = regexp.MustCompile(`[^a-z0-9\-.]`)

func sanitizeNodeName(name string) string {
	retVal := strings.ToLower(name)
	retVal = nonAlphanumDash.ReplaceAllString(retVal, "-")
	retVal = strings.Trim(retVal, "-.")
	return retVal
}

func heartbeat(ctx context.Context, clientset *kubernetes.Clientset, nodeName string) {
	ticker := time.NewTicker(heartbeatSec * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				log.G(ctx).WithError(err).Warn("Failed to get node for heartbeat")
				continue
			}

			for i := range node.Status.Conditions {
				if node.Status.Conditions[i].Type == corev1.NodeReady {
					node.Status.Conditions[i].LastHeartbeatTime = metav1.Now()
					node.Status.Conditions[i].Status = corev1.ConditionTrue
				}
			}

			if _, err = clientset.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
				log.G(ctx).WithError(err).Warn("Failed to update node heartbeat")
			}
		}
	}
}
