package hypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/containerd/log"
	"github.com/creack/pty"
)

// AppleVM implements the Hypervisor interface using the macOS `container` CLI.
type AppleVM struct{}

// NewAppleVM creates a new AppleVM backed by the `container` CLI.
func NewAppleVM() *AppleVM {
	return &AppleVM{}
}

func (a *AppleVM) Run(ctx context.Context, id string, image string, args []string, opts RunOpts) (*ContainerInfo, error) {
	cmdArgs := []string{"run", "--name", id}

	if opts.Detach {
		cmdArgs = append(cmdArgs, "-d")
	}
	if opts.CPUs > 0 {
		cmdArgs = append(cmdArgs, "-c", fmt.Sprintf("%d", opts.CPUs))
	}
	if opts.Memory != "" {
		cmdArgs = append(cmdArgs, "-m", opts.Memory)
	}
	if opts.Workdir != "" {
		cmdArgs = append(cmdArgs, "-w", opts.Workdir)
	}
	for k, v := range opts.Env {
		cmdArgs = append(cmdArgs, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	for k, v := range opts.Labels {
		cmdArgs = append(cmdArgs, "-l", fmt.Sprintf("%s=%s", k, v))
	}
	for _, m := range opts.Mounts {
		cmdArgs = append(cmdArgs, "-v", fmt.Sprintf("%s:%s", m.Source, m.Target))
	}
	for _, p := range opts.Ports {
		cmdArgs = append(cmdArgs, "-p", p)
	}

	cmdArgs = append(cmdArgs, image)
	cmdArgs = append(cmdArgs, args...)

	log.G(ctx).WithField("args", cmdArgs).Debug("container run")

	output, err := exec.CommandContext(ctx, "container", cmdArgs...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("container run: %w\noutput: %s", err, string(output))
	}

	retVal, err := a.Inspect(ctx, id)
	if err != nil {
		retVal = &ContainerInfo{ID: id, State: StateRunning}
	}
	return retVal, nil
}

func (a *AppleVM) Stop(ctx context.Context, id string) error {
	output, err := exec.CommandContext(ctx, "container", "stop", id).CombinedOutput()
	if err != nil {
		return fmt.Errorf("container stop %s: %w\noutput: %s", id, err, string(output))
	}
	return nil
}

func (a *AppleVM) Remove(ctx context.Context, id string) error {
	output, err := exec.CommandContext(ctx, "container", "rm", id).CombinedOutput()
	if err != nil {
		return fmt.Errorf("container rm %s: %w\noutput: %s", id, err, string(output))
	}
	return nil
}

func (a *AppleVM) Exec(ctx context.Context, id string, args []string) ([]byte, error) {
	cmdArgs := make([]string, 0, 2+len(args))
	cmdArgs = append(cmdArgs, "exec", id)
	cmdArgs = append(cmdArgs, args...)

	retVal, err := exec.CommandContext(ctx, "container", cmdArgs...).CombinedOutput()
	if err != nil {
		return retVal, fmt.Errorf("container exec %s: %w\noutput: %s", id, err, string(retVal))
	}
	return retVal, nil
}

func (a *AppleVM) ExecInteractive(ctx context.Context, id string, args []string, stdin io.Reader, stdout, stderr io.Writer, tty bool) error {
	cmdArgs := make([]string, 0, 4+len(args))
	cmdArgs = append(cmdArgs, "exec")
	if stdin != nil {
		cmdArgs = append(cmdArgs, "-i")
	}
	if tty {
		cmdArgs = append(cmdArgs, "-t")
	}
	cmdArgs = append(cmdArgs, id)
	cmdArgs = append(cmdArgs, args...)

	log.G(ctx).WithField("args", cmdArgs).Debug("container exec interactive")

	cmd := exec.CommandContext(ctx, "container", cmdArgs...)

	if tty {
		return a.execWithPTY(cmd, stdin, stdout)
	}

	cmd.Stdin = stdin
	cmd.Stdout = stdout
	if stderr != nil {
		cmd.Stderr = stderr
	} else {
		cmd.Stderr = stdout
	}
	return cmd.Run()
}

// execWithPTY allocates a local pseudo-terminal so the `container exec -t`
// CLI sees a real TTY, then relays I/O between the PTY and the SPDY streams.
func (a *AppleVM) execWithPTY(cmd *exec.Cmd, stdin io.Reader, stdout io.Writer) error {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("pty start: %w", err)
	}
	defer ptmx.Close()

	// Relay SPDY stdin → PTY
	if stdin != nil {
		go func() {
			_, _ = io.Copy(ptmx, stdin)
			// stdin closed — nothing more to do; the command will exit on its own.
		}()
	}

	// Relay PTY → SPDY stdout (stderr is merged by the PTY)
	_, _ = io.Copy(stdout, ptmx)

	return cmd.Wait()
}

func (a *AppleVM) Inspect(ctx context.Context, id string) (*ContainerInfo, error) {
	output, err := exec.CommandContext(ctx, "container", "inspect", id).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("container inspect %s: %w\noutput: %s", id, err, string(output))
	}

	var inspections []struct {
		Configuration struct {
			ID string `json:"id"`
		} `json:"configuration"`
		Status   string `json:"status"`
		Networks []struct {
			IPv4Address string `json:"ipv4Address"`
			Hostname    string `json:"hostname"`
		} `json:"networks"`
	}

	if err := json.Unmarshal(output, &inspections); err != nil {
		return nil, fmt.Errorf("parsing inspect output: %w", err)
	}
	if len(inspections) == 0 {
		return nil, fmt.Errorf("no inspection data for %s", id)
	}

	insp := inspections[0]
	retVal := &ContainerInfo{
		ID:    id,
		State: insp.Status,
	}
	// The Apple container CLI doesn't expose exit codes via inspect.
	// Assume non-zero for stopped/exited containers so restart policies work.
	if insp.Status == StateStopped || insp.Status == StateExited {
		retVal.ExitCode = 1
	}
	if len(insp.Networks) > 0 {
		retVal.IP = strings.Split(insp.Networks[0].IPv4Address, "/")[0]
		retVal.Hostname = insp.Networks[0].Hostname
	}
	return retVal, nil
}

func (a *AppleVM) Kill(ctx context.Context, id string, signal string) error {
	cmdArgs := []string{"kill"}
	if signal != "" {
		cmdArgs = append(cmdArgs, "-s", signal)
	}
	cmdArgs = append(cmdArgs, id)

	output, err := exec.CommandContext(ctx, "container", cmdArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("container kill %s: %w\noutput: %s", id, err, string(output))
	}
	return nil
}

func (a *AppleVM) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	cmdArgs := []string{"logs"}
	if follow {
		cmdArgs = append(cmdArgs, "-f")
	}
	cmdArgs = append(cmdArgs, id)

	cmd := exec.CommandContext(ctx, "container", cmdArgs...)

	// Use an OS pipe so both stdout and stderr are captured — some container
	// runtimes write log content (or errors) to stderr.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("container logs %s: creating pipe: %w", id, err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("container logs %s: %w", id, err)
	}

	// Close our copy of the write end so reads get EOF when the command exits.
	pw.Close()

	retVal := &cmdReadCloser{ReadCloser: pr, cmd: cmd}
	return retVal, nil
}

// cmdReadCloser wraps a command's stdout pipe and kills the process on Close.
type cmdReadCloser struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (c *cmdReadCloser) Close() error {
	err := c.ReadCloser.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return err
}
