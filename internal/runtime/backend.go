package runtime

import (
	"context"
	"fmt"
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
	// Configuration is a request-scoped complete runtime document. Backends use
	// it instead of generated disk state for input-bearing job operations.
	Configuration []byte
}

// DefaultLogTail is the number of last lines a log read returns, or a follow
// replays, when the request names none, for both runtimes.
const DefaultLogTail = 200

// MaxLocalFollowTail is the largest backlog a process-compose follow is asked
// to replay. `process logs --follow` (v1.120.0 to v1.122.0) replays a backlog
// over a websocket that drops lines and disconnects once a client falls about
// 256 lines behind, so a longer replay loses the newest lines. A buffered read
// goes through the supervisor's log range endpoint and has no such limit.
const MaxLocalFollowTail = 256

// MaxLogTail caps the tail of a log read.
const MaxLogTail = 10000

// ProcessNotLoadedError reports a process the process-compose supervisor has
// not loaded, such as a service the stack declared after the supervisor
// started. Replacing marks a process the supervisor is replacing with an
// updated definition: it drops the process from its log endpoint until the old
// one has stopped, so the read is worth retrying and the supervisor needs no
// restart.
type ProcessNotLoadedError struct {
	Name        string
	ControlPort int
	Replacing   bool
}

func (e *ProcessNotLoadedError) Error() string {
	if e.Replacing {
		return fmt.Sprintf("process-compose supervisor on 127.0.0.1:%d is replacing %s with its updated definition; retry once it has restarted", e.ControlPort, e.Name)
	}
	return fmt.Sprintf("process-compose supervisor on 127.0.0.1:%d has not loaded %s; restart the supervisor to load processes the stack declared after it started", e.ControlPort, e.Name)
}

type LogsRequest struct {
	Root     string
	Services []string
	Follow   bool
	EnvFile  string
	MaxBytes int
	// NoPrefix asks the backend to omit its per-line service-name prefix
	// (docker compose `--no-log-prefix`). Used for single-service streams
	// where the service is already known from the request. Backends without
	// such a prefix ignore it.
	NoPrefix bool
	// Tail, when > 0, limits a read, or the backlog a follow stream replays
	// before it continues live, to the last N lines (docker compose /
	// process-compose `--tail`). Zero omits the flag and defers to the
	// backend's default backlog.
	Tail        int
	ControlPort int
}

type StatusRequest struct {
	Root string
	// EnvFile is the stack env file whose current values the backend exports
	// to the status command, so `docker compose ps` interpolates the compose
	// file from the same fresh values as every other compose invocation.
	EnvFile       string
	ControlPort   int
	Configuration []byte
	// All includes stopped and never-started containers (`docker compose ps
	// --all`), so a finished job and a crashed service report their exit
	// instead of vanishing. process-compose always lists every process.
	All bool
}

type ServiceStatus struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
	State   string `json:"state"`
	// Health mirrors docker's healthcheck verdict (`healthy`,
	// `unhealthy`, `starting`). Empty when the container has no
	// healthcheck declared or when the backend doesn't expose one.
	Health   string `json:"health,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	// Restarts is how many times the runtime's restart policy has restarted
	// the process since it was last started. process-compose reports it;
	// docker compose's status does not.
	Restarts   int    `json:"restarts,omitempty"`
	PID        int    `json:"pid,omitempty"`
	Generation string `json:"generation,omitempty"`
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
	// StreamLogs is the live-follow counterpart to Logs: it yields one line
	// per channel element as the runtime produces it, rather than buffering
	// the whole output and delivering it on exit. Used for log subscriptions
	// and per-service log sockets; Logs remains the bounded-read path.
	StreamLogs(ctx context.Context, req LogsRequest) (<-chan string, error)
	Status(ctx context.Context, req StatusRequest) ([]ServiceStatus, error)
}

// JobBackend executes a declared one-shot workload as a runtime-managed
// instance, preserving terminal state for native dependency conditions.
type JobBackend interface {
	RunJob(ctx context.Context, target Target, job JobSpec) ([]byte, error)
}

type JobSpec struct {
	Name          string
	Configuration []byte
}

// ApplyBackend recreates selected services from the current generated runtime
// document without traversing dependencies already ordered by the caller.
type ApplyBackend interface {
	Apply(ctx context.Context, target Target) error
}
