package provider

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/containerd/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/containerd-shim-applevm-v2/pkg/hypervisor"
)

//region members
type probeRunner struct {
	hv                hypervisor.Hypervisor
	successCountByKey map[string]int32
	failureCountByKey map[string]int32
}

//endregion

//region public methods

func newProbeRunner(hv hypervisor.Hypervisor) *probeRunner {
	retVal := &probeRunner{
		hv:                hv,
		successCountByKey: make(map[string]int32),
		failureCountByKey: make(map[string]int32),
	}
	return retVal
}

// checkReadiness returns true if the container's readiness probe passes (or if no probe is defined).
func (r *probeRunner) checkReadiness(ctx context.Context, pod *corev1.Pod, containerIdx int, containerIP string) bool {
	spec := &pod.Spec.Containers[containerIdx]
	probe := spec.ReadinessProbe
	if probe == nil {
		return true
	}

	key := probeKey(pod, spec.Name, "readiness")
	retVal := r.evaluateProbe(ctx, pod, spec, probe, containerIP, key)
	return retVal
}

// checkLiveness returns true if the container's liveness probe passes (or if no probe is defined).
func (r *probeRunner) checkLiveness(ctx context.Context, pod *corev1.Pod, containerIdx int, containerIP string) bool {
	spec := &pod.Spec.Containers[containerIdx]
	probe := spec.LivenessProbe
	if probe == nil {
		return true
	}

	key := probeKey(pod, spec.Name, "liveness")
	retVal := r.evaluateProbe(ctx, pod, spec, probe, containerIP, key)
	return retVal
}

// clearProbeState removes all probe state for a given container (e.g. after restart).
func (r *probeRunner) clearProbeState(pod *corev1.Pod, containerName string) {
	for _, probeType := range []string{"readiness", "liveness"} {
		key := probeKey(pod, containerName, probeType)
		delete(r.successCountByKey, key)
		delete(r.failureCountByKey, key)
	}
}

//endregion

//region private methods

func (r *probeRunner) evaluateProbe(ctx context.Context, pod *corev1.Pod, spec *corev1.Container, probe *corev1.Probe, containerIP string, key string) bool {
	initialDelay := time.Duration(probe.InitialDelaySeconds) * time.Second
	if initialDelay > 0 {
		// Skip probe if container hasn't been running long enough.
		// We use the pod start time as an approximation of container start time.
		if pod.Status.StartTime != nil {
			elapsed := time.Since(pod.Status.StartTime.Time)
			if elapsed < initialDelay {
				return r.successCountByKey[key] > 0
			}
		}
	}

	cID := containerID(pod, spec.Name)
	passed := r.runProbe(ctx, cID, probe, containerIP)

	successThreshold := probe.SuccessThreshold
	if successThreshold <= 0 {
		successThreshold = 1
	}
	failureThreshold := probe.FailureThreshold
	if failureThreshold <= 0 {
		failureThreshold = 3
	}

	if passed {
		r.successCountByKey[key]++
		r.failureCountByKey[key] = 0
	} else {
		r.failureCountByKey[key]++
		r.successCountByKey[key] = 0
	}

	retVal := r.successCountByKey[key] >= successThreshold && r.failureCountByKey[key] < failureThreshold
	return retVal
}

func (r *probeRunner) runProbe(ctx context.Context, cID string, probe *corev1.Probe, containerIP string) bool {
	timeout := time.Duration(probe.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 1 * time.Second
	}

	switch {
	case probe.Exec != nil:
		return r.execProbe(ctx, cID, probe.Exec, timeout)
	case probe.HTTPGet != nil:
		return r.httpGetProbe(ctx, probe.HTTPGet, containerIP, timeout)
	case probe.TCPSocket != nil:
		return r.tcpSocketProbe(probe.TCPSocket, containerIP, timeout)
	default:
		return true
	}
}

func (r *probeRunner) execProbe(ctx context.Context, cID string, action *corev1.ExecAction, timeout time.Duration) bool {
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	_, err := r.hv.Exec(execCtx, cID, action.Command)
	return err == nil
}

func (r *probeRunner) httpGetProbe(ctx context.Context, action *corev1.HTTPGetAction, containerIP string, timeout time.Duration) bool {
	port := resolvePort(action.Port)
	scheme := string(corev1.URISchemeHTTP)
	if action.Scheme != "" {
		scheme = string(action.Scheme)
	}

	url := fmt.Sprintf("%s://%s:%d%s", scheme, containerIP, port, action.Path)

	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}

	for _, h := range action.HTTPHeaders {
		req.Header.Set(h.Name, h.Value)
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	retVal := resp.StatusCode >= 200 && resp.StatusCode < 400
	return retVal
}

func (r *probeRunner) tcpSocketProbe(action *corev1.TCPSocketAction, containerIP string, timeout time.Duration) bool {
	port := resolvePort(action.Port)
	addr := net.JoinHostPort(containerIP, fmt.Sprintf("%d", port))

	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func resolvePort(port intstr.IntOrString) int32 {
	if port.Type == intstr.Int {
		return port.IntVal
	}
	// Named ports are not resolved here; log a warning if encountered.
	return 0
}

func probeKey(pod *corev1.Pod, containerName string, probeType string) string {
	return fmt.Sprintf("%s/%s/%s/%s", pod.Namespace, pod.Name, containerName, probeType)
}

func logProbeFailure(ctx context.Context, pod *corev1.Pod, containerName string, probeType string) {
	log.G(ctx).WithField("pod", podKey(pod)).WithField("container", containerName).
		Infof("%s probe failed threshold", probeType)
}

//endregion
