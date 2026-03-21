//go:build e2e

package e2e

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestPodOnAppleVM tests deploying a pod to the Apple VM virtual kubelet node.
func TestPodOnAppleVM(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not found in PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Check the virtual node exists
	cmd := exec.CommandContext(ctx, "kubectl", "get", "node", "-l", "node.kubernetes.io/applevm=true", "-o", "name")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Skip("No Apple VM virtual node found in cluster")
	}

	podYAML := `
apiVersion: v1
kind: Pod
metadata:
  name: test-applevm-e2e
  namespace: default
spec:
  tolerations:
  - key: "virtual-kubelet.io/provider"
    value: "applevm"
    effect: "NoSchedule"
  nodeSelector:
    node.kubernetes.io/applevm: "true"
  containers:
  - name: test
    image: docker.io/library/alpine:latest
    command: ["sleep", "60"]
`

	// Clean up first
	exec.CommandContext(ctx, "kubectl", "delete", "pod", "test-applevm-e2e", "--ignore-not-found").Run()

	// Apply pod
	applyCmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	applyCmd.Stdin = strings.NewReader(podYAML)
	out, err = applyCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl apply failed: %v\n%s", err, string(out))
	}

	defer exec.Command("kubectl", "delete", "pod", "test-applevm-e2e", "--ignore-not-found").Run()

	// Wait for pod to be running
	for i := 0; i < 30; i++ {
		cmd := exec.CommandContext(ctx, "kubectl", "get", "pod", "test-applevm-e2e", "-o", "jsonpath={.status.phase}")
		out, _ := cmd.CombinedOutput()
		if string(out) == "Running" {
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Verify
	cmd = exec.CommandContext(ctx, "kubectl", "get", "pod", "test-applevm-e2e", "-o", "wide")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl get pod failed: %v\n%s", err, string(out))
	}

	t.Logf("Pod status:\n%s", string(out))

	if !strings.Contains(string(out), "Running") {
		t.Errorf("expected pod to be Running")
	}

	// Should also show in `container list`
	if _, err := exec.LookPath("container"); err == nil {
		listCmd := exec.CommandContext(ctx, "container", "list")
		listOut, _ := listCmd.CombinedOutput()
		t.Logf("Apple container list:\n%s", string(listOut))
	}
}
