//go:build integration

package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestContainerRunAndInspect verifies the Apple container CLI works.
func TestContainerRunAndInspect(t *testing.T) {
	if _, err := exec.LookPath("container"); err != nil {
		t.Skip("Apple container CLI not found")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	name := "test-integration-lifecycle"

	// Clean up
	exec.CommandContext(ctx, "container", "stop", name).Run()
	exec.CommandContext(ctx, "container", "rm", name).Run()

	// Run detached
	cmd := exec.CommandContext(ctx, "container", "run", "-d", "--name", name, "docker.io/library/alpine:latest", "sleep", "30")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("container run failed: %v\n%s", err, string(out))
	}

	defer func() {
		exec.Command("container", "stop", name).Run()
		exec.Command("container", "rm", name).Run()
	}()

	// Inspect
	time.Sleep(2 * time.Second)
	inspectCmd := exec.CommandContext(ctx, "container", "inspect", name)
	inspectOut, err := inspectCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("container inspect failed: %v\n%s", err, string(inspectOut))
	}

	if !strings.Contains(string(inspectOut), `"status":"running"`) {
		t.Errorf("expected running status, got: %s", string(inspectOut))
	}

	// Check container appears in list
	listCmd := exec.CommandContext(ctx, "container", "list")
	listOut, err := listCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("container list failed: %v\n%s", err, string(listOut))
	}

	if !strings.Contains(string(listOut), name) {
		t.Errorf("expected container %s in list, got: %s", name, string(listOut))
	}

	t.Logf("Container running with inspect output:\n%s", string(inspectOut))
}
