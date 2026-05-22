package runtime

import (
	"context"
	"io"
)

type Target struct {
	Root        string
	Services    []string
	Build       bool
	EnvFile     string
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
	Status(ctx context.Context, root string) ([]ServiceStatus, error)
}
