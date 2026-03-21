package provider

import (
	"context"
	"time"

	"github.com/containerd/log"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/containerd-shim-applevm-v2/pkg/hypervisor"
)

// Controller watches the Kubernetes API for pods assigned to our virtual node
// and dispatches them to the AppleVMProvider.
type Controller struct {
	clientset *kubernetes.Clientset
	provider  *AppleVMProvider
	nodeName  string
	ctx       context.Context
}

// NewController creates a controller for managing pods on this virtual node.
func NewController(clientset *kubernetes.Clientset, provider *AppleVMProvider, nodeName string) *Controller {
	retVal := &Controller{
		clientset: clientset,
		provider:  provider,
		nodeName:  nodeName,
	}
	return retVal
}

// Run starts watching for pods assigned to this node.
func (c *Controller) Run(ctx context.Context) {
	c.ctx = ctx
	log.G(ctx).WithField("node", c.nodeName).Info("Starting pod controller")

	watchList := cache.NewListWatchFromClient(
		c.clientset.CoreV1().RESTClient(),
		"pods",
		corev1.NamespaceAll,
		fields.OneTermEqualSelector("spec.nodeName", c.nodeName),
	)

	_, informer := cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: watchList,
		ObjectType:    &corev1.Pod{},
		ResyncPeriod:  30 * time.Second,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onAdd,
			UpdateFunc: c.onUpdate,
			DeleteFunc: c.onDelete,
		},
	})

	go c.periodicStatusSync(ctx)

	informer.Run(ctx.Done())
}

func (c *Controller) onAdd(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}

	key := pod.Namespace + "/" + pod.Name
	log.G(c.ctx).WithField("pod", key).Info("Pod added")

	// If the pod has a DeletionTimestamp (evicted while we were down), clean up.
	if pod.DeletionTimestamp != nil {
		log.G(c.ctx).WithField("pod", key).Info("Pod marked for deletion during downtime, cleaning up")
		if err := c.provider.DeletePod(c.ctx, pod); err != nil {
			log.G(c.ctx).WithError(err).Error("Failed to delete evicted pod")
		}
		gracePeriod := int64(0)
		if err := c.clientset.CoreV1().Pods(pod.Namespace).Delete(c.ctx, pod.Name, metav1.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
		}); err != nil && !kerrors.IsNotFound(err) {
			log.G(c.ctx).WithError(err).Error("Failed to force-delete evicted pod from API")
		}
		return
	}

	// Always check if the backing container exists, regardless of pod phase.
	// After a kubelet restart, Kubernetes may have changed the pod phase
	// (e.g. to Failed due to unreachable taint) even though the container
	// is still running.
	cID := containerID(pod, pod.Spec.Containers[0].Name)
	info, err := c.provider.hv.Inspect(c.ctx, cID)
	if err == nil && info.State == hypervisor.StateRunning {
		log.G(c.ctx).WithField("pod", key).Info("Backing container still running, reconciling")
		c.reconcilePod(pod)
		return
	}

	if err := c.provider.CreatePod(c.ctx, pod); err != nil {
		log.G(c.ctx).WithError(err).Error("Failed to create pod")
		c.updatePodStatus(pod, corev1.PodFailed, err.Error())
		return
	}

	c.syncPodStatus(pod)
}

func (c *Controller) onUpdate(_, newObj any) {
	pod, ok := newObj.(*corev1.Pod)
	if !ok {
		return
	}

	if pod.DeletionTimestamp != nil {
		key := pod.Namespace + "/" + pod.Name
		// Skip if we already cleaned up this pod
		if !c.provider.hasPod(key) {
			return
		}
		log.G(c.ctx).WithField("pod", key).Info("Pod marked for deletion")
		if err := c.provider.DeletePod(c.ctx, pod); err != nil {
			log.G(c.ctx).WithError(err).Error("Failed to delete pod")
		}
		// Force-remove from API server so the pod doesn't stay in Terminating
		gracePeriod := int64(0)
		if err := c.clientset.CoreV1().Pods(pod.Namespace).Delete(c.ctx, pod.Name, metav1.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
		}); err != nil && !kerrors.IsNotFound(err) {
			log.G(c.ctx).WithError(err).Error("Failed to force-delete pod from API")
		}
		return
	}

	if err := c.provider.UpdatePod(c.ctx, pod); err != nil {
		log.G(c.ctx).WithError(err).Error("Failed to update pod")
	}
}

func (c *Controller) onDelete(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}

	key := pod.Namespace + "/" + pod.Name
	// Skip if we already cleaned up this pod (e.g. via onUpdate with DeletionTimestamp)
	if !c.provider.hasPod(key) {
		return
	}

	log.G(c.ctx).WithField("pod", key).Info("Pod deleted")

	if err := c.provider.DeletePod(c.ctx, pod); err != nil {
		log.G(c.ctx).WithError(err).Error("Failed to delete pod")
	}
}

func (c *Controller) reconcilePod(pod *corev1.Pod) {
	key := pod.Namespace + "/" + pod.Name

	// Verify all containers exist in the Apple container runtime.
	for _, container := range pod.Spec.Containers {
		cID := containerID(pod, container.Name)
		_, err := c.provider.hv.Inspect(c.ctx, cID)
		if err != nil {
			log.G(c.ctx).WithField("pod", key).WithField("container", container.Name).
				Info("Stale pod detected, backing container not found after kubelet restart")
			c.updatePodStatus(pod, corev1.PodFailed, "backing container not found after kubelet restart")
			return
		}
	}

	// All containers exist — re-register in the provider's tracking map.
	// Reset phase to Running since the backing containers are alive.
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Message = ""
	pod.Status.Reason = ""

	c.provider.mu.Lock()
	c.provider.podsByKey[key] = pod.DeepCopy()
	c.provider.mu.Unlock()

	// Push real container state to the API server.
	c.syncPodStatus(pod)
	log.G(c.ctx).WithField("pod", key).Info("Pod reconciled after kubelet restart")
}

func (c *Controller) periodicStatusSync(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pods, err := c.provider.GetPods(ctx)
			if err != nil {
				continue
			}
			for _, pod := range pods {
				c.handleRestarts(ctx, pod)
				c.syncPodStatus(pod)
			}
		}
	}
}

func (c *Controller) handleRestarts(ctx context.Context, pod *corev1.Pod) {
	policy := pod.Spec.RestartPolicy
	if policy == corev1.RestartPolicyNever {
		return
	}

	now := metav1.Now()
	statuses := c.provider.inspectContainerStatuses(ctx, pod, now)

	for i, cs := range statuses {
		spec := &pod.Spec.Containers[i]
		cID := containerID(pod, spec.Name)

		// Check liveness probes for running containers.
		if cs.State.Running != nil {
			info, err := c.provider.hv.Inspect(ctx, cID)
			if err == nil {
				if !c.provider.probes.checkLiveness(ctx, pod, i, info.IP) {
					logProbeFailure(ctx, pod, spec.Name, "Liveness")
					if err := c.provider.hv.Kill(ctx, cID, "SIGKILL"); err != nil {
						log.G(ctx).WithError(err).WithField("container", cID).Error("Failed to kill container after liveness failure")
					}
				}
			}
			continue
		}

		if cs.State.Terminated == nil {
			continue
		}

		shouldRestart := policy == corev1.RestartPolicyAlways ||
			(policy == corev1.RestartPolicyOnFailure && cs.State.Terminated.ExitCode != 0)
		if !shouldRestart {
			continue
		}

		if c.provider.shouldBackoff(cID) {
			log.G(ctx).WithField("pod", podKey(pod)).WithField("container", spec.Name).
				Debug("Container in CrashLoopBackOff, skipping restart")
			continue
		}

		log.G(ctx).WithField("pod", podKey(pod)).WithField("container", spec.Name).
			Info("Restarting terminated container")

		if err := c.provider.restartContainer(ctx, pod, spec); err != nil {
			log.G(ctx).WithError(err).Error("Failed to restart container")
		}
	}
}

func (c *Controller) updatePodStatus(pod *corev1.Pod, phase corev1.PodPhase, message string) {
	pod.Status.Phase = phase
	pod.Status.Message = message
	_, err := c.clientset.CoreV1().Pods(pod.Namespace).UpdateStatus(c.ctx, pod, metav1.UpdateOptions{})
	if err != nil {
		log.G(c.ctx).WithError(err).Error("Failed to update pod status")
	}
}

func (c *Controller) syncPodStatus(pod *corev1.Pod) {
	status, err := c.provider.GetPodStatus(c.ctx, pod.Namespace, pod.Name)
	if err != nil {
		log.G(c.ctx).WithError(err).Error("Failed to get pod status")
		return
	}

	pod.Status = *status
	_, err = c.clientset.CoreV1().Pods(pod.Namespace).UpdateStatus(c.ctx, pod, metav1.UpdateOptions{})
	if err != nil {
		log.G(c.ctx).WithError(err).Error("Failed to sync pod status")
	}
}
