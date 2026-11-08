// Package restic is the typed process adapter for restic-backed stack
// snapshots. It owns restic's command line, JSON, streams, credentials, and
// exit statuses; higher layers own snapshot ordering and policy decisions.
package restic

import (
	"context"
	"errors"
	"io"
	"time"
)

const (
	// MinVersion is the oldest restic release supported by the adapter.
	MinVersion = "0.19.1"
	// InstallPackage is the pinned go install target offered for a missing
	// restic binary.
	InstallPackage = "github.com/restic/restic/cmd/restic@v0.19.1"
)

// Kind classifies a failed restic invocation for the service layer.
type Kind string

const (
	// NotFound means the repository or requested snapshot was not found.
	NotFound Kind = "not_found"
	// Locked means another restic process holds an incompatible lock.
	Locked Kind = "locked"
	// WrongPassword means restic could not decrypt the repository.
	WrongPassword Kind = "wrong_password"
	// WriteFailure means restic could not read all source data. It includes an
	// empty --stdin backup, for which restic creates an incomplete snapshot.
	WriteFailure Kind = "write_failure"
	// Interrupted means restic exited for an interrupt or the caller's context.
	Interrupted Kind = "interrupted"
	// Unavailable means restic is missing or below the supported version floor.
	Unavailable Kind = "unavailable"
	// Failure is every other restic or adapter failure.
	Failure Kind = "failure"
)

// Error is a redacted restic or adapter failure. An unreachable REST backend
// can retry inside restic for roughly 15 minutes before exiting 1, so the
// caller's context deadline is the only reliable bound on that case.
type Error struct {
	Kind     Kind
	ExitCode int
	Message  string
	Hint     string
	err      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	message := e.Message
	if message == "" {
		message = "restic failed"
	}
	if e.Hint == "" {
		return message
	}
	return message + "; " + e.Hint
}

// Unwrap returns the underlying process, context, reader, or writer error.
func (e *Error) Unwrap() error { return e.err }

// IsKind reports whether err contains an adapter Error of kind.
func IsKind(err error, kind Kind) bool {
	var resticErr *Error
	return errors.As(err, &resticErr) && resticErr.Kind == kind
}

// Repository is one restic repository and its child-process-only
// credentials. CacheDir is required for repository operations so the caller,
// not restic's shared default, owns cache placement. Backend credentials must
// be passed explicitly through Env; the adapter allowlists ambient variables.
type Repository struct {
	URL      string
	Password string
	Env      []string
	CacheDir string
}

// VersionInfo is the JSON returned by `restic version`.
type VersionInfo struct {
	Version   string `json:"version"`
	GoVersion string `json:"go_version"`
	GoOS      string `json:"go_os"`
	GoArch    string `json:"go_arch"`
}

// InitOptions configures repository initialization. CopyChunkerFrom asks
// restic to copy chunker parameters from the source repository.
type InitOptions struct {
	Repository      Repository
	CopyChunkerFrom *Repository
}

// InitResult identifies an initialized repository.
type InitResult struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
}

// KeyAddOptions adds a second password-backed key to a repository.
type KeyAddOptions struct {
	Repository  Repository
	NewPassword string
	User        string
	Host        string
}

// Key identifies a newly added repository key.
type Key struct {
	ID string
}

// Config is the repository configuration returned by `restic cat config`.
type Config struct {
	Version           int    `json:"version"`
	ID                string `json:"id"`
	ChunkerPolynomial string `json:"chunker_polynomial"`
}

// Summary is restic's backup summary. Unknown future JSON fields are ignored.
type Summary struct {
	FilesNew            int       `json:"files_new"`
	FilesChanged        int       `json:"files_changed"`
	FilesUnmodified     int       `json:"files_unmodified"`
	DirsNew             int       `json:"dirs_new"`
	DirsChanged         int       `json:"dirs_changed"`
	DirsUnmodified      int       `json:"dirs_unmodified"`
	DataBlobs           int       `json:"data_blobs"`
	TreeBlobs           int       `json:"tree_blobs"`
	DataAdded           int64     `json:"data_added"`
	DataAddedPacked     int64     `json:"data_added_packed"`
	TotalFilesProcessed int       `json:"total_files_processed"`
	TotalBytesProcessed int64     `json:"total_bytes_processed"`
	TotalDuration       float64   `json:"total_duration"`
	BackupStart         time.Time `json:"backup_start"`
	BackupEnd           time.Time `json:"backup_end"`
	SnapshotID          string    `json:"snapshot_id"`
}

// BackupResult is returned even when a reader error follows a committed
// partial stdin snapshot, allowing the caller to forget SnapshotID.
type BackupResult struct {
	SnapshotID string
	Summary    Summary
}

// BackupStdinOptions configures a single streamed file snapshot. When
// ExpectedBytes is non-nil, a different byte count is returned as an error
// alongside any snapshot ID restic committed.
type BackupStdinOptions struct {
	Repository    Repository
	Filename      string
	Tags          []string
	Host          string
	ExpectedBytes *int64
}

// BackupPathsOptions configures a file-tree snapshot. Parent, when set, must
// be an exact full snapshot ID.
type BackupPathsOptions struct {
	Repository  Repository
	BaseDir     string
	Paths       []string
	Tags        []string
	Host        string
	Parent      string
	IgnoreInode bool
}

// SnapshotSummary is the summary embedded in snapshot metadata.
type SnapshotSummary struct {
	BackupStart         time.Time `json:"backup_start"`
	BackupEnd           time.Time `json:"backup_end"`
	FilesNew            int       `json:"files_new"`
	FilesChanged        int       `json:"files_changed"`
	FilesUnmodified     int       `json:"files_unmodified"`
	DirsNew             int       `json:"dirs_new"`
	DirsChanged         int       `json:"dirs_changed"`
	DirsUnmodified      int       `json:"dirs_unmodified"`
	DataBlobs           int       `json:"data_blobs"`
	TreeBlobs           int       `json:"tree_blobs"`
	DataAdded           int64     `json:"data_added"`
	DataAddedPacked     int64     `json:"data_added_packed"`
	TotalFilesProcessed int       `json:"total_files_processed"`
	TotalBytesProcessed int64     `json:"total_bytes_processed"`
}

// Snapshot is one restic snapshot.
type Snapshot struct {
	Time           time.Time       `json:"time"`
	Parent         string          `json:"parent"`
	Tree           string          `json:"tree"`
	Paths          []string        `json:"paths"`
	Hostname       string          `json:"hostname"`
	Username       string          `json:"username"`
	UID            int             `json:"uid"`
	GID            int             `json:"gid"`
	Tags           []string        `json:"tags"`
	Original       string          `json:"original"`
	ProgramVersion string          `json:"program_version"`
	Summary        SnapshotSummary `json:"summary"`
	ID             string          `json:"id"`
	ShortID        string          `json:"short_id"`
}

// SnapshotFilter selects snapshots. Each inner Tags slice is an AND set;
// repeated outer groups are ORed by restic.
type SnapshotFilter struct {
	Tags  [][]string
	Host  string
	Paths []string
}

// SnapshotsOptions configures snapshot listing.
type SnapshotsOptions struct {
	Repository Repository
	Filter     SnapshotFilter
}

// LsOptions configures a snapshot tree listing.
type LsOptions struct {
	Repository Repository
	SnapshotID string
	Paths      []string
	Recursive  bool
}

// Node is one `restic ls --json` tree node.
type Node struct {
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Path        string    `json:"path"`
	UID         int       `json:"uid"`
	GID         int       `json:"gid"`
	Size        int64     `json:"size"`
	Mode        uint32    `json:"mode"`
	Permissions string    `json:"permissions"`
	MTime       time.Time `json:"mtime"`
	ATime       time.Time `json:"atime"`
	CTime       time.Time `json:"ctime"`
	Inode       uint64    `json:"inode"`
}

// LsResult contains the snapshot header and listed nodes.
type LsResult struct {
	Snapshot Snapshot
	Nodes    []Node
}

// ArchiveFormat is a directory archive produced by Dump.
type ArchiveFormat string

const (
	// ArchiveTar writes a tar archive.
	ArchiveTar ArchiveFormat = "tar"
	// ArchiveZip writes a zip archive.
	ArchiveZip ArchiveFormat = "zip"
)

// DumpOptions configures a streamed snapshot dump. Archive is empty for raw
// file bytes and tar or zip for a directory.
type DumpOptions struct {
	Repository Repository
	SnapshotID string
	Path       string
	Archive    ArchiveFormat
}

// RestoreOptions configures filesystem restoration.
type RestoreOptions struct {
	Repository Repository
	SnapshotID string
	Target     string
	Includes   []string
}

// RestoreSummary is restic's restore summary.
type RestoreSummary struct {
	TotalFiles    int   `json:"total_files"`
	FilesRestored int   `json:"files_restored"`
	TotalBytes    int64 `json:"total_bytes"`
	BytesRestored int64 `json:"bytes_restored"`
}

// RetentionPolicy is the arithmetic restic applies during a dry-run forget.
type RetentionPolicy struct {
	KeepLast          int
	KeepHourly        int
	KeepDaily         int
	KeepWeekly        int
	KeepMonthly       int
	KeepYearly        int
	KeepWithin        string
	KeepWithinHourly  string
	KeepWithinDaily   string
	KeepWithinWeekly  string
	KeepWithinMonthly string
	KeepWithinYearly  string
}

// ForgetDryRunOptions previews a retention policy without deleting snapshots.
type ForgetDryRunOptions struct {
	Repository Repository
	Filter     SnapshotFilter
	Policy     RetentionPolicy
}

// ForgetReason says why a snapshot is kept by a retention policy.
type ForgetReason struct {
	Snapshot Snapshot `json:"snapshot"`
	Matches  []string `json:"matches"`
}

// ForgetGroup is one restic retention group.
type ForgetGroup struct {
	Tags    []string       `json:"tags"`
	Host    string         `json:"host"`
	Paths   []string       `json:"paths"`
	Keep    []Snapshot     `json:"keep"`
	Remove  []Snapshot     `json:"remove"`
	Reasons []ForgetReason `json:"reasons"`
}

// ForgetOptions deletes only the explicit snapshot IDs in IDs.
type ForgetOptions struct {
	Repository Repository
	IDs        []string
}

// TextResult is redacted plain output from a restic command whose --json mode
// does not define a machine-readable result.
type TextResult struct {
	Output string
}

// CheckOptions configures a repository integrity check.
type CheckOptions struct {
	Repository     Repository
	ReadDataSubset string
}

// CheckSummary is restic's integrity-check summary.
type CheckSummary struct {
	NumErrors          int      `json:"num_errors"`
	BrokenPacks        []string `json:"broken_packs"`
	SuggestRepairIndex bool     `json:"suggest_repair_index"`
	SuggestPrune       bool     `json:"suggest_prune"`
}

// CopyOptions copies exact full snapshot IDs from Source to Destination.
type CopyOptions struct {
	Destination Repository
	Source      Repository
	IDs         []string
}

// CopyResult lists matched destination snapshots. Original preserves restic's
// root provenance, so after copy-of-copy it may name an ancestor rather than
// the requested source ID.
type CopyResult struct {
	Snapshots []Snapshot
}

// Runner is the process execution seam used by Client.
type Runner interface {
	Run(context.Context, Command) (CommandResult, error)
}

// Command describes one child process. Passwords are written to inherited
// file descriptors beginning at fd 3 by ExecRunner and must never be logged.
type Command struct {
	Name      string
	Dir       string
	Args      []string
	Env       []string
	Stdin     io.Reader
	Stdout    io.Writer
	Passwords []string
	// Secrets are exact values ExecRunner must remove from trace diagnostics,
	// including values too short for logctx's process-wide redactor.
	Secrets []string
}

// CommandResult contains separately captured standard output and error.
type CommandResult struct {
	Stdout       []byte
	Stderr       []byte
	StdinBytes   int64
	StdinError   error
	StdinAborted bool
	secrets      []string
}
