package provider

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/containerd/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/containerd-shim-applevm-v2/config"
	"github.com/containerd-shim-applevm-v2/pkg/hypervisor"
)

// maxBackoff is the cap for CrashLoopBackOff delay (matches real kubelet).
const maxBackoff = 5 * time.Minute

// AppleVMProvider implements the virtual-kubelet PodLifecycleHandler interface.
// It delegates pod execution to the macOS `container` CLI.
type AppleVMProvider struct {
	hv                        hypervisor.Hypervisor
	clientset                 kubernetes.Interface
	nodeName                  string
	cfg                       *config.Config
	podsByKey                 map[string]*corev1.Pod
	mu                        sync.RWMutex
	notifyFn                  func(*corev1.Pod)
	restartCountByContainerID map[string]int32
	lastRestartByContainerID  map[string]time.Time
	prevLogsByContainerID     map[string][]byte
	lastStateByContainerID    map[string]corev1.ContainerState
	probes                    *probeRunner
}

// New creates a new AppleVMProvider.
func New(nodeName string, cfg *config.Config, clientset kubernetes.Interface) *AppleVMProvider {
	retVal := &AppleVMProvider{
		hv:                        hypervisor.NewAppleVM(),
		clientset:                 clientset,
		nodeName:                  nodeName,
		cfg:                       cfg,
		podsByKey:                 make(map[string]*corev1.Pod),
		restartCountByContainerID: make(map[string]int32),
		lastRestartByContainerID:  make(map[string]time.Time),
		prevLogsByContainerID:     make(map[string][]byte),
		lastStateByContainerID:    make(map[string]corev1.ContainerState),
	}
	retVal.probes = newProbeRunner(retVal.hv)
	return retVal
}

// CreatePod creates containers for each container spec in the pod via `container run`.
func (p *AppleVMProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	log.G(ctx).WithField("pod", podKey(pod)).Info("CreatePod")

	// Run init containers sequentially; each must succeed before the next.
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		cName := containerID(pod, c.Name)

		// Remove any leftover container from a previous attempt.
		_ = p.hv.Remove(ctx, cName)

		opts, args := p.buildRunOpts(pod, c)
		opts.Detach = false // init containers run to completion

		log.G(ctx).WithField("initContainer", c.Name).Info("Running init container")
		_, err := p.hv.Run(ctx, cName, c.Image, args, opts)
		if err != nil {
			p.cleanupPodContainers(ctx, pod)
			return fmt.Errorf("init container %s failed: %w", c.Name, err)
		}

		// Clean up the finished init container.
		_ = p.hv.Remove(ctx, cName)
		log.G(ctx).WithField("initContainer", c.Name).Info("Init container completed")
	}

	// Run all main containers and collect their info.
	infoByCName := make(map[string]*hypervisor.ContainerInfo, len(pod.Spec.Containers))

	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		cName := containerID(pod, c.Name)

		// Remove any leftover container from a previous attempt.
		_ = p.hv.Remove(ctx, cName)

		opts, args := p.buildRunOpts(pod, c)

		info, err := p.hv.Run(ctx, cName, c.Image, args, opts)
		if err != nil {
			p.cleanupPodContainers(ctx, pod)
			return fmt.Errorf("starting container %s: %w", c.Name, err)
		}

		infoByCName[c.Name] = info
		log.G(ctx).WithField("container", c.Name).WithField("ip", info.IP).Info("Container VM started")
	}

	now := metav1.Now()

	podReadyStatus := corev1.ConditionTrue
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].ReadinessProbe != nil {
			podReadyStatus = corev1.ConditionFalse
			break
		}
	}

	pod.Status = corev1.PodStatus{
		Phase:     corev1.PodRunning,
		StartTime: &now,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: podReadyStatus, LastTransitionTime: now},
			{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: now},
		},
		ContainerStatuses:     buildStatusesFromInfo(pod, infoByCName, now),
		InitContainerStatuses: buildInitStatuses(pod, now),
	}

	// Set pod IP from the first container
	if first := infoByCName[pod.Spec.Containers[0].Name]; first != nil && first.IP != "" {
		pod.Status.PodIP = first.IP
		pod.Status.PodIPs = []corev1.PodIP{{IP: first.IP}}
	}

	p.mu.Lock()
	p.podsByKey[podKey(pod)] = pod.DeepCopy()
	p.mu.Unlock()

	p.notify(pod)
	return nil
}

// UpdatePod updates a pod (no-op — Apple container CLI doesn't support live updates).
func (p *AppleVMProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	log.G(ctx).WithField("pod", podKey(pod)).Debug("UpdatePod")

	p.mu.Lock()
	p.podsByKey[podKey(pod)] = pod.DeepCopy()
	p.mu.Unlock()

	return nil
}

// DeletePod stops and removes all containers for a pod.
func (p *AppleVMProvider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	log.G(ctx).WithField("pod", podKey(pod)).Info("DeletePod")

	p.cleanupPodContainers(ctx, pod)

	now := metav1.Now()
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.Reason = "Deleted"
	for i := range pod.Status.ContainerStatuses {
		pod.Status.ContainerStatuses[i].State = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, Reason: "Deleted", FinishedAt: now,
			},
		}
		pod.Status.ContainerStatuses[i].Ready = false
	}

	p.notify(pod)

	p.mu.Lock()
	delete(p.podsByKey, podKey(pod))
	p.mu.Unlock()

	return nil
}

// GetPod returns a pod by namespace and name.
func (p *AppleVMProvider) GetPod(_ context.Context, namespace, name string) (*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	key := namespace + "/" + name
	pod, ok := p.podsByKey[key]
	if !ok {
		return nil, fmt.Errorf("pod %s not found", key)
	}
	return pod.DeepCopy(), nil
}

// GetPodStatus returns the current status of a pod by querying `container inspect`.
func (p *AppleVMProvider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	p.mu.RLock()
	pod, ok := p.podsByKey[namespace+"/"+name]
	p.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("pod %s/%s not found", namespace, name)
	}

	now := metav1.Now()
	pod.Status.ContainerStatuses = p.inspectContainerStatuses(ctx, pod, now)

	allRunning := true
	allReady := true
	anyTerminated := false
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running == nil {
			allRunning = false
		}
		if !cs.Ready {
			allReady = false
		}
		if cs.State.Terminated != nil {
			anyTerminated = true
		}
	}
	if allRunning {
		pod.Status.Phase = corev1.PodRunning
	} else if anyTerminated {
		pod.Status.Phase = corev1.PodFailed
	}

	// Update PodReady condition based on container readiness.
	podReadyStatus := corev1.ConditionFalse
	if allReady && allRunning {
		podReadyStatus = corev1.ConditionTrue
	}
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodReady {
			pod.Status.Conditions[i].Status = podReadyStatus
			pod.Status.Conditions[i].LastTransitionTime = now
			break
		}
	}

	return pod.Status.DeepCopy(), nil
}

// GetPods returns all pods managed by this provider.
func (p *AppleVMProvider) GetPods(_ context.Context) ([]*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	retVal := make([]*corev1.Pod, 0, len(p.podsByKey))
	for _, pod := range p.podsByKey {
		retVal = append(retVal, pod.DeepCopy())
	}
	return retVal, nil
}

// NotifyPods registers the callback for async pod status updates.
func (p *AppleVMProvider) NotifyPods(_ context.Context, cb func(*corev1.Pod)) {
	p.mu.Lock()
	p.notifyFn = cb
	p.mu.Unlock()
}

func (p *AppleVMProvider) notify(pod *corev1.Pod) {
	p.mu.RLock()
	cb := p.notifyFn
	p.mu.RUnlock()
	if cb != nil {
		cb(pod.DeepCopy())
	}
}

// buildStatusesFromInfo constructs container statuses from already-fetched info (no extra execs).
func buildStatusesFromInfo(pod *corev1.Pod, infoByCName map[string]*hypervisor.ContainerInfo, now metav1.Time) []corev1.ContainerStatus {
	retVal := make([]corev1.ContainerStatus, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		info := infoByCName[c.Name]
		cs := corev1.ContainerStatus{
			Name:        c.Name,
			Image:       c.Image,
			ContainerID: "applevm://" + containerID(pod, c.Name),
		}
		if info != nil && info.State == hypervisor.StateRunning {
			cs.State = corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{StartedAt: now},
			}
			cs.Ready = c.ReadinessProbe == nil
		} else {
			cs.State = corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
			}
		}
		retVal = append(retVal, cs)
	}
	return retVal
}

// buildInitStatuses constructs completed statuses for init containers (they already ran).
func buildInitStatuses(pod *corev1.Pod, now metav1.Time) []corev1.ContainerStatus {
	retVal := make([]corev1.ContainerStatus, 0, len(pod.Spec.InitContainers))
	for _, c := range pod.Spec.InitContainers {
		cs := corev1.ContainerStatus{
			Name:        c.Name,
			Image:       c.Image,
			ContainerID: "applevm://" + containerID(pod, c.Name),
			Ready:       true,
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode:   0,
					FinishedAt: now,
					Reason:     "Completed",
				},
			},
		}
		retVal = append(retVal, cs)
	}
	return retVal
}

// inspectContainerStatuses calls `container inspect` per container (used for status polling).
func (p *AppleVMProvider) inspectContainerStatuses(ctx context.Context, pod *corev1.Pod, now metav1.Time) []corev1.ContainerStatus {
	retVal := make([]corev1.ContainerStatus, 0, len(pod.Spec.Containers))
	for i, c := range pod.Spec.Containers {
		cID := containerID(pod, c.Name)
		cs := corev1.ContainerStatus{
			Name:        c.Name,
			Image:       c.Image,
			ContainerID: "applevm://" + cID,
		}

		p.mu.RLock()
		cs.RestartCount = p.restartCountByContainerID[cID]
		cs.LastTerminationState = p.lastStateByContainerID[cID]
		p.mu.RUnlock()

		info, err := p.hv.Inspect(ctx, cID)
		if err != nil {
			cs.State = corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "InspectFailed"},
			}
		} else {
			switch info.State {
			case hypervisor.StateRunning:
				cs.State = corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{StartedAt: now},
				}
				cs.Ready = p.probes.checkReadiness(ctx, pod, i, info.IP)
			case hypervisor.StateStopped, hypervisor.StateExited:
				if p.shouldBackoff(cID) {
					cs.State = corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					}
				} else {
					cs.State = corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode:   info.ExitCode,
							FinishedAt: now,
						},
					}
				}
			default:
				cs.State = corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: info.State},
				}
			}
		}

		retVal = append(retVal, cs)
	}
	return retVal
}

func (p *AppleVMProvider) buildRunOpts(pod *corev1.Pod, c *corev1.Container) (hypervisor.RunOpts, []string) {
	memoryMiB := p.cfg.Resources.MemoryMiB
	if memoryMiB == 0 {
		memoryMiB = config.DefaultMemoryMiB
	}
	cpus := p.cfg.Resources.VCPUs

	// Override from container resource limits if specified.
	if memLimit, ok := c.Resources.Limits["memory"]; ok {
		memoryMiB = uint64(memLimit.Value() / (1024 * 1024))
	}
	if cpuLimit, ok := c.Resources.Limits["cpu"]; ok {
		cpus = uint(cpuLimit.Value())
	}

	opts := hypervisor.RunOpts{
		Detach: true,
		CPUs:   cpus,
		Memory: fmt.Sprintf("%dMiB", memoryMiB),
		Env:    make(map[string]string),
		Labels: map[string]string{
			"applevm.pod.namespace": pod.Namespace,
			"applevm.pod.name":      pod.Name,
			"applevm.container":     c.Name,
		},
	}

	// Resolve envFrom (ConfigMap/Secret references) first, so explicit Env can override.
	p.resolveEnvFrom(pod.Namespace, c.EnvFrom, opts.Env)

	for _, e := range c.Env {
		if e.ValueFrom != nil {
			val := p.resolveEnvValueFrom(pod.Namespace, e.ValueFrom)
			if val != "" {
				opts.Env[e.Name] = val
			}
		} else {
			opts.Env[e.Name] = e.Value
		}
	}

	if c.WorkingDir != "" {
		opts.Workdir = c.WorkingDir
	}
	for _, port := range c.Ports {
		opts.Ports = append(opts.Ports, fmt.Sprintf("%d:%d", port.ContainerPort, port.ContainerPort))
	}

	// Resolve volume mounts.
	hostPathByVolName := p.resolveVolumes(pod)
	for _, vm := range c.VolumeMounts {
		if hostPath, ok := hostPathByVolName[vm.Name]; ok {
			opts.Mounts = append(opts.Mounts, hypervisor.Mount{
				Source: hostPath,
				Target: vm.MountPath,
			})
		}
	}

	args := make([]string, 0, len(c.Command)+len(c.Args))
	args = append(args, c.Command...)
	args = append(args, c.Args...)

	return opts, args
}

func (p *AppleVMProvider) resolveEnvFrom(namespace string, envFrom []corev1.EnvFromSource, env map[string]string) {
	ctx := context.Background()
	for _, src := range envFrom {
		if src.ConfigMapRef != nil {
			cm, err := p.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, src.ConfigMapRef.Name, metav1.GetOptions{})
			if err != nil {
				if src.ConfigMapRef.Optional != nil && *src.ConfigMapRef.Optional {
					continue
				}
				log.G(ctx).WithError(err).WithField("configmap", src.ConfigMapRef.Name).Warn("Failed to resolve envFrom configMap")
				continue
			}
			for k, v := range cm.Data {
				env[src.Prefix+k] = v
			}
		}
		if src.SecretRef != nil {
			secret, err := p.clientset.CoreV1().Secrets(namespace).Get(ctx, src.SecretRef.Name, metav1.GetOptions{})
			if err != nil {
				if src.SecretRef.Optional != nil && *src.SecretRef.Optional {
					continue
				}
				log.G(ctx).WithError(err).WithField("secret", src.SecretRef.Name).Warn("Failed to resolve envFrom secret")
				continue
			}
			for k, v := range secret.Data {
				env[src.Prefix+k] = string(v)
			}
		}
	}
}

func (p *AppleVMProvider) resolveEnvValueFrom(namespace string, src *corev1.EnvVarSource) string {
	ctx := context.Background()

	if src.ConfigMapKeyRef != nil {
		cm, err := p.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, src.ConfigMapKeyRef.Name, metav1.GetOptions{})
		if err != nil {
			return ""
		}
		return cm.Data[src.ConfigMapKeyRef.Key]
	}

	if src.SecretKeyRef != nil {
		secret, err := p.clientset.CoreV1().Secrets(namespace).Get(ctx, src.SecretKeyRef.Name, metav1.GetOptions{})
		if err != nil {
			return ""
		}
		return string(secret.Data[src.SecretKeyRef.Key])
	}

	if src.FieldRef != nil {
		return resolveFieldRef(src.FieldRef.FieldPath, namespace)
	}

	return ""
}

func resolveFieldRef(fieldPath, namespace string) string {
	switch fieldPath {
	case "metadata.namespace":
		return namespace
	default:
		return ""
	}
}

// resolveVolumes builds a map of volume name → host path for all supported volume types.
func (p *AppleVMProvider) resolveVolumes(pod *corev1.Pod) map[string]string {
	retVal := make(map[string]string, len(pod.Spec.Volumes))
	ctx := context.Background()
	podDir := filepath.Join(os.TempDir(), "applevm-volumes", pod.Namespace, pod.Name)

	for _, vol := range pod.Spec.Volumes {
		switch {
		case vol.EmptyDir != nil:
			dir := filepath.Join(podDir, vol.Name)
			_ = os.MkdirAll(dir, 0o755)
			retVal[vol.Name] = dir

		case vol.HostPath != nil:
			retVal[vol.Name] = vol.HostPath.Path

		case vol.ConfigMap != nil:
			dir := filepath.Join(podDir, vol.Name)
			_ = os.MkdirAll(dir, 0o755)
			cm, err := p.clientset.CoreV1().ConfigMaps(pod.Namespace).Get(ctx, vol.ConfigMap.Name, metav1.GetOptions{})
			if err != nil {
				log.G(ctx).WithError(err).WithField("configmap", vol.ConfigMap.Name).Warn("Failed to resolve configMap volume")
				continue
			}
			for k, v := range cm.Data {
				_ = os.WriteFile(filepath.Join(dir, k), []byte(v), 0o644)
			}
			retVal[vol.Name] = dir

		case vol.Secret != nil:
			dir := filepath.Join(podDir, vol.Name)
			_ = os.MkdirAll(dir, 0o755)
			secret, err := p.clientset.CoreV1().Secrets(pod.Namespace).Get(ctx, vol.Secret.SecretName, metav1.GetOptions{})
			if err != nil {
				log.G(ctx).WithError(err).WithField("secret", vol.Secret.SecretName).Warn("Failed to resolve secret volume")
				continue
			}
			for k, v := range secret.Data {
				_ = os.WriteFile(filepath.Join(dir, k), v, 0o644)
			}
			retVal[vol.Name] = dir
		}
	}
	return retVal
}

func (p *AppleVMProvider) restartContainer(ctx context.Context, pod *corev1.Pod, c *corev1.Container) error {
	cID := containerID(pod, c.Name)

	// Capture previous logs and terminated state before removing.
	p.capturePreviousState(ctx, cID)

	if err := p.hv.Remove(ctx, cID); err != nil {
		log.G(ctx).WithError(err).WithField("container", cID).Debug("remove failed before restart")
	}

	opts, args := p.buildRunOpts(pod, c)
	_, err := p.hv.Run(ctx, cID, c.Image, args, opts)
	if err != nil {
		return fmt.Errorf("restarting container %s: %w", c.Name, err)
	}

	p.probes.clearProbeState(pod, c.Name)

	now := time.Now()
	p.mu.Lock()
	p.restartCountByContainerID[cID]++
	p.lastRestartByContainerID[cID] = now
	p.mu.Unlock()

	log.G(ctx).WithField("container", cID).Info("Container restarted")
	return nil
}

func (p *AppleVMProvider) capturePreviousState(ctx context.Context, cID string) {
	// Save logs from the dying container.
	rc, err := p.hv.Logs(ctx, cID, false)
	if err == nil {
		data, _ := io.ReadAll(rc)
		rc.Close()
		p.mu.Lock()
		p.prevLogsByContainerID[cID] = data
		p.mu.Unlock()
	}

	// Save terminated state.
	info, err := p.hv.Inspect(ctx, cID)
	if err == nil && (info.State == hypervisor.StateStopped || info.State == hypervisor.StateExited) {
		state := corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   info.ExitCode,
				FinishedAt: metav1.Now(),
			},
		}
		p.mu.Lock()
		p.lastStateByContainerID[cID] = state
		p.mu.Unlock()
	}
}

// PreviousLogs returns the saved logs from the previous container instance.
func (p *AppleVMProvider) PreviousLogs(cID string) []byte {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.prevLogsByContainerID[cID]
}

// backoffDelay returns the backoff duration for a container based on its restart count.
// Uses exponential backoff: 10s, 20s, 40s, 80s, 160s, 300s (capped).
func (p *AppleVMProvider) backoffDelay(cID string) time.Duration {
	p.mu.RLock()
	count := p.restartCountByContainerID[cID]
	p.mu.RUnlock()

	if count == 0 {
		return 0
	}

	delay := 10 * time.Second
	for i := int32(1); i < count; i++ {
		delay *= 2
		if delay >= maxBackoff {
			return maxBackoff
		}
	}
	return delay
}

// shouldBackoff returns true if the container is still within its backoff window.
func (p *AppleVMProvider) shouldBackoff(cID string) bool {
	p.mu.RLock()
	lastRestart := p.lastRestartByContainerID[cID]
	p.mu.RUnlock()

	if lastRestart.IsZero() {
		return false
	}

	return time.Since(lastRestart) < p.backoffDelay(cID)
}

func (p *AppleVMProvider) cleanupPodContainers(ctx context.Context, pod *corev1.Pod) {
	// Use a context that won't be cancelled by the parent — cleanup must
	// finish even if the informer or kubelet context is cancelled (e.g. after
	// force-deleting the pod from the API server).
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, c := range pod.Spec.Containers {
		wg.Add(1)
		go func(cID string) {
			defer wg.Done()
			p.stopAndRemoveContainer(cleanupCtx, cID)
		}(containerID(pod, c.Name))
	}
	wg.Wait()

	// Clean up volume dirs.
	podDir := filepath.Join(os.TempDir(), "applevm-volumes", pod.Namespace, pod.Name)
	_ = os.RemoveAll(podDir)
}

// stopAndRemoveContainer stops a container, waits for it to actually stop, and
// then removes it. Falls back to SIGKILL if the graceful stop doesn't work.
func (p *AppleVMProvider) stopAndRemoveContainer(ctx context.Context, cID string) {
	logger := log.G(ctx).WithField("container", cID)

	// Try graceful stop first.
	if err := p.hv.Stop(ctx, cID); err != nil {
		logger.WithError(err).Warn("graceful stop failed, attempting SIGKILL")
		if err := p.hv.Kill(ctx, cID, "SIGKILL"); err != nil {
			logger.WithError(err).Warn("SIGKILL failed")
		}
	}

	// Wait for the container to actually stop before removing.
	if !p.waitForStop(ctx, cID, 10*time.Second) {
		logger.Warn("container did not stop in time, forcing removal")
	}

	// Remove the container, retrying once on failure.
	if err := p.hv.Remove(ctx, cID); err != nil {
		logger.WithError(err).Warn("first remove attempt failed, retrying")
		time.Sleep(1 * time.Second)
		if err := p.hv.Remove(ctx, cID); err != nil {
			logger.WithError(err).Error("failed to remove container after retry")
		}
	}
}

// waitForStop polls container state until it is no longer running or the
// timeout expires. Returns true if the container stopped.
func (p *AppleVMProvider) waitForStop(ctx context.Context, cID string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return false
		case <-ctx.Done():
			return false
		case <-ticker.C:
			info, err := p.hv.Inspect(ctx, cID)
			if err != nil {
				// Container already gone.
				return true
			}
			if info.State != hypervisor.StateRunning {
				return true
			}
		}
	}
}

func (p *AppleVMProvider) hasPod(key string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.podsByKey[key]
	return ok
}

func podKey(pod *corev1.Pod) string {
	return pod.Namespace + "/" + pod.Name
}

func containerID(pod *corev1.Pod, containerName string) string {
	return ContainerIDFromNames(pod.Namespace, pod.Name, containerName)
}

// ContainerIDFromNames builds the container CLI ID from namespace, pod, and container names.
func ContainerIDFromNames(namespace, podName, containerName string) string {
	name := fmt.Sprintf("%s-%s-%s", namespace, podName, containerName)
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, ".", "-")
	return name
}
