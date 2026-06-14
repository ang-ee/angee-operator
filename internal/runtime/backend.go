package runtime

import (
	"context"
	"io"
	"time"
)

// GracefulWaitDelay bounds how long a foreground runtime process has to exit
// after being interrupted before exec force-kills it. Shared by the compose and
// process-compose foreground runners so the grace period can't drift between
// them.
const GracefulWaitDelay = 10 * time.Second

type Target struct {
	Root     string
	Services []string
	Build    bool
	EnvFile  string
	// Attached, when set on a foreground Up, keeps the supervisor in the
	// foreground streaming logs instead of detaching (`docker compose up`
	// without `-d`). Used by `angee dev` so container logs interleave with
	// the process-compose stream. Ignored by backends that always stream
	// in the foreground (process-compose).
	Attached    bool
	ControlPort int
}

type LogsRequest struct {
	Root        string
	Services    []string
	Follow      bool
	EnvFile     string
	MaxBytes    int
	ControlPort int
}

type StatusRequest struct {
	Root        string
	ControlPort int
}

type ServiceStatus struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
	State   string `json:"state"`
	// Health mirrors docker's healthcheck verdict (`healthy`,
	// `unhealthy`, `starting`). Empty when the container has no
	// healthcheck declared or when the backend doesn't expose one.
	Health string `json:"health,omitempty"`
}

type Backend interface {
	Build(ctx context.Context, target Target) error
	Up(ctx context.Context, target Target) error
	UpForeground(ctx context.Context, target Target, stdout io.Writer, stderr io.Writer) error
	Down(ctx context.Context, target Target) error
	Start(ctx context.Context, target Target) error
	Stop(ctx context.Context, target Target) error
	Restart(ctx context.Context, target Target) error
	Logs(ctx context.Context, req LogsRequest) (<-chan string, error)
	Status(ctx context.Context, req StatusRequest) ([]ServiceStatus, error)
}
