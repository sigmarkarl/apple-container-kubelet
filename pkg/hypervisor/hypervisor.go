package hypervisor

import (
	"context"
	"io"
)

// Container state constants returned by the Apple container CLI.
const (
	StateRunning = "running"
	StateStopped = "stopped"
	StateExited  = "exited"
)

// ContainerInfo holds runtime info returned by the Apple container system.
type ContainerInfo struct {
	ID       string
	State    string
	IP       string
	Hostname string
	ExitCode int32
}

// Hypervisor abstracts the Apple container runtime backend.
type Hypervisor interface {
	Run(ctx context.Context, id string, image string, args []string, opts RunOpts) (*ContainerInfo, error)
	Stop(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error
	Exec(ctx context.Context, id string, args []string) ([]byte, error)
	ExecInteractive(ctx context.Context, id string, args []string, stdin io.Reader, stdout, stderr io.Writer, tty bool) error
	Inspect(ctx context.Context, id string) (*ContainerInfo, error)
	Kill(ctx context.Context, id string, signal string) error
	Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
}

// RunOpts configures a container VM.
type RunOpts struct {
	Detach  bool
	Env     map[string]string
	Mounts  []Mount
	CPUs    uint
	Memory  string
	Labels  map[string]string
	Ports   []string
	Workdir string
}

// Mount represents a volume mount.
type Mount struct {
	Source string
	Target string
}
