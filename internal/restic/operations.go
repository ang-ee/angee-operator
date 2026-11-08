package restic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ang-ee/angee-operator/internal/logctx"
)

// Version returns restic's build information and refuses releases below
// MinVersion.
func (c *Client) Version(ctx context.Context) (VersionInfo, error) {
	result, err := c.run(ctx, invocation{args: []string{"version"}, metadata: true})
	if err != nil {
		return VersionInfo{}, err
	}
	var wire struct {
		MessageType string `json:"message_type"`
		VersionInfo
	}
	if err := decodeJSON(result.Stdout, &wire); err != nil {
		return VersionInfo{}, adapterError(Failure, -1, err.Error(), "Run `restic version --json` to inspect the installed binary.", err)
	}
	if !VersionAtLeast(wire.Version, MinVersion) {
		message := fmt.Sprintf("restic %s is older than required %s", wire.Version, MinVersion)
		return VersionInfo{}, adapterError(Unavailable, -1, message, "Install it with `go install "+InstallPackage+"`.", nil)
	}
	return wire.VersionInfo, nil
}

// Init initializes a repository, optionally copying source chunker parameters.
// Both repository passwords use anonymous inherited file descriptors.
func (c *Client) Init(ctx context.Context, options InitOptions) (InitResult, error) {
	call := invocation{repository: &options.Repository, args: []string{"init"}}
	if options.CopyChunkerFrom != nil {
		call.source = options.CopyChunkerFrom
		call.args = append(call.args, "--copy-chunker-params", "--from-password-file", "/dev/fd/4")
	}
	result, err := c.run(ctx, call)
	if err != nil {
		return InitResult{}, err
	}
	var wire struct {
		MessageType string `json:"message_type"`
		InitResult
	}
	if err := decodeJSON(result.Stdout, &wire); err != nil {
		return InitResult{}, adapterError(Failure, -1, err.Error(), "Inspect restic's initialization output.", err)
	}
	wire.Repository = redactSecrets(wire.Repository, result.secrets)
	return wire.InitResult, nil
}

var keyIDPattern = regexp.MustCompile(`(?m)saved new key with ID ([0-9a-f]{64})`)

// KeyAdd adds a key whose new password is supplied on fd 4.
func (c *Client) KeyAdd(ctx context.Context, options KeyAddOptions) (Key, error) {
	if options.NewPassword == "" {
		return Key{}, adapterError(Failure, -1, "new restic key password is empty", "Pass a non-empty new password.", nil)
	}
	if strings.TrimSpace(options.NewPassword) != options.NewPassword || strings.ContainsAny(options.NewPassword, "\r\n") {
		return Key{}, adapterError(Failure, -1, "new restic key password has leading or trailing whitespace", "Pass a password without surrounding whitespace or line breaks.", nil)
	}
	if err := validateHost(options.Host); err != nil {
		return Key{}, err
	}
	args := []string{"key", "add", "--new-password-file", "/dev/fd/4"}
	if options.User != "" {
		args = append(args, "--user", options.User)
	}
	if options.Host != "" {
		args = append(args, "--host", options.Host)
	}
	result, err := c.run(ctx, invocation{
		repository: &options.Repository,
		args:       args,
		metadata:   true,
		retryLock:  true,
		passwords:  []string{options.NewPassword},
	})
	if err != nil {
		return Key{}, err
	}
	match := keyIDPattern.FindSubmatch(append(result.Stdout, result.Stderr...))
	if match == nil {
		return Key{}, adapterError(Failure, -1, "restic key add returned no key ID", "Inspect restic's key output.", nil)
	}
	return Key{ID: string(match[1])}, nil
}

// CatConfig returns the repository's typed configuration.
func (c *Client) CatConfig(ctx context.Context, repository Repository) (Config, error) {
	result, err := c.run(ctx, invocation{
		repository: &repository,
		args:       []string{"cat", "config"},
		metadata:   true,
		noLock:     true,
	})
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := decodeJSON(result.Stdout, &config); err != nil {
		return Config{}, adapterError(Failure, -1, err.Error(), "Inspect restic's repository configuration.", err)
	}
	return config, nil
}

// BackupStdin streams reader directly to restic as one nested snapshot file.
// It never stages bytes on disk. Restic cannot detect a producer that ends
// cleanly too early, so callers should set ExpectedBytes when they know the
// length. If reader itself fails after restic commits, BackupResult and the
// reader error are returned together so the caller can forget the partial
// snapshot. The runner asks restic to abort when Read fails. A reader that
// implements io.Closer is always closed by the runner once restic exits, so
// Close must return promptly. If Read remains blocked after Close, the call
// can still return but its copier goroutine remains until Read does; P4 passes
// a closable pipe whose Close unblocks Read.
func (c *Client) BackupStdin(ctx context.Context, options BackupStdinOptions, reader io.Reader) (BackupResult, error) {
	if err := validateSnapshotFilename(options.Filename); err != nil {
		return BackupResult{}, err
	}
	if reader == nil {
		return BackupResult{}, adapterError(Failure, -1, "backup input is nil", "Pass the snapshot member's stream reader.", nil)
	}
	if options.ExpectedBytes != nil && *options.ExpectedBytes < 0 {
		return BackupResult{}, adapterError(Failure, -1, "expected backup input length is negative", "Pass a non-negative expected byte count.", nil)
	}
	if err := validateSnapshotMetadata(options.Tags, options.Host); err != nil {
		return BackupResult{}, err
	}
	args := []string{"backup", "--stdin", "--stdin-filename", options.Filename}
	args = appendBackupMetadata(args, options.Tags, options.Host, "")
	raw, runErr := c.run(ctx, invocation{
		repository: &options.Repository,
		args:       args,
		stdin:      reader,
		retryLock:  true,
	})
	summary, summaryErr := backupSummary(raw.Stdout)
	backup := BackupResult{SnapshotID: summary.SnapshotID, Summary: summary}
	if runErr != nil {
		if ctx.Err() == nil && raw.StdinAborted && raw.StdinError != nil {
			return backup, streamError(backup.SnapshotID, raw.StdinError, runErr, raw.secrets)
		}
		return backup, runErr
	}
	if raw.StdinError != nil {
		return backup, streamError(backup.SnapshotID, raw.StdinError, nil, raw.secrets)
	}
	if options.ExpectedBytes != nil && raw.StdinBytes != *options.ExpectedBytes {
		streamErr := fmt.Errorf("backup input length %d does not match expected %d", raw.StdinBytes, *options.ExpectedBytes)
		return backup, streamError(backup.SnapshotID, streamErr, nil, raw.secrets)
	}
	if summaryErr != nil {
		return BackupResult{}, adapterError(Failure, -1, summaryErr.Error(), "Inspect restic's backup output.", summaryErr)
	}
	return backup, nil
}

// BackupPaths snapshots relative Paths with BaseDir as the child working
// directory, producing stable /<group>/<name>/... snapshot trees. Absolute,
// cleaned-different, and parent-traversing arguments are refused. Restic
// stores symlinks inside the selected tree as links, but follows a symlinked
// ancestor while resolving an argument; confinement is the caller's job.
func (c *Client) BackupPaths(ctx context.Context, options BackupPathsOptions) (BackupResult, error) {
	if !filepath.IsAbs(options.BaseDir) {
		return BackupResult{}, adapterError(Failure, -1, "backup base directory must be absolute", "Pass an absolute staging or stack base directory.", nil)
	}
	if len(options.Paths) == 0 {
		return BackupResult{}, adapterError(Failure, -1, "backup paths are empty", "Pass at least one relative member path.", nil)
	}
	for _, path := range options.Paths {
		if err := validateRelativePath(path); err != nil {
			return BackupResult{}, err
		}
	}
	if err := validateSnapshotMetadata(options.Tags, options.Host); err != nil {
		return BackupResult{}, err
	}
	if options.Parent != "" {
		if err := validateSnapshotID("backup parent", options.Parent); err != nil {
			return BackupResult{}, err
		}
	}
	args := appendBackupMetadata([]string{"backup"}, options.Tags, options.Host, options.Parent)
	if options.IgnoreInode {
		args = append(args, "--ignore-inode")
	}
	args = append(args, "--")
	args = append(args, options.Paths...)
	raw, runErr := c.run(ctx, invocation{
		repository: &options.Repository,
		dir:        options.BaseDir,
		args:       args,
		retryLock:  true,
	})
	summary, summaryErr := backupSummary(raw.Stdout)
	backup := BackupResult{SnapshotID: summary.SnapshotID, Summary: summary}
	if runErr != nil {
		return backup, runErr
	}
	if summaryErr != nil {
		return BackupResult{}, adapterError(Failure, -1, summaryErr.Error(), "Inspect restic's backup output.", summaryErr)
	}
	return backup, nil
}

func appendBackupMetadata(args []string, tags []string, host string, parent string) []string {
	for _, tag := range tags {
		args = append(args, "--tag", tag)
	}
	if host != "" {
		args = append(args, "--host", host)
	}
	if parent != "" {
		args = append(args, "--parent", parent)
	}
	return args
}

func streamError(snapshotID string, streamErr error, runErr error, secrets []string) *Error {
	message := "read backup input: " + redactSecrets(streamErr.Error(), secrets)
	if snapshotID != "" {
		message += "; restic committed snapshot " + snapshotID
	}
	return adapterError(Failure, -1, message, "Forget any committed partial snapshot before retrying the member.", errors.Join(runErr, streamErr))
}

// Snapshots returns snapshots matching restic's AND-within, OR-between tag
// groups plus optional host and path filters.
func (c *Client) Snapshots(ctx context.Context, options SnapshotsOptions) ([]Snapshot, error) {
	args, err := appendSnapshotFilter([]string{"snapshots"}, options.Filter)
	if err != nil {
		return nil, err
	}
	result, err := c.run(ctx, invocation{
		repository: &options.Repository,
		args:       args,
		metadata:   true,
		noLock:     true,
	})
	if err != nil {
		return nil, err
	}
	var snapshots []Snapshot
	if err := decodeJSON(result.Stdout, &snapshots); err != nil {
		return nil, adapterError(Failure, -1, err.Error(), "Inspect restic's snapshot list.", err)
	}
	return snapshots, nil
}

func appendSnapshotFilter(args []string, filter SnapshotFilter) ([]string, error) {
	for _, group := range filter.Tags {
		if len(group) == 0 {
			return nil, adapterError(Failure, -1, "snapshot tag filter contains an empty group", "Pass one or more valid tags in each OR group.", nil)
		}
		if err := validateTags(group); err != nil {
			return nil, err
		}
		args = append(args, "--tag", strings.Join(group, ","))
	}
	if filter.Host != "" {
		if err := validateHost(filter.Host); err != nil {
			return nil, err
		}
		args = append(args, "--host", filter.Host)
	}
	for _, path := range filter.Paths {
		args = append(args, "--path", path)
	}
	return args, nil
}

// Ls returns a snapshot header and its selected tree nodes.
func (c *Client) Ls(ctx context.Context, options LsOptions) (LsResult, error) {
	if err := validateSnapshotID("list", options.SnapshotID); err != nil {
		return LsResult{}, err
	}
	args := []string{"ls"}
	if options.Recursive {
		args = append(args, "--recursive")
	}
	args = append(args, "--", options.SnapshotID)
	args = append(args, options.Paths...)
	raw, err := c.run(ctx, invocation{
		repository: &options.Repository,
		args:       args,
		metadata:   true,
		noLock:     true,
	})
	if err != nil {
		return LsResult{}, err
	}
	var listed LsResult
	parseErr := scanJSONLines(raw.Stdout, func(header jsonHeader, data []byte) error {
		if header.StructType == "node" || header.MessageType == "node" {
			var node Node
			if err := json.Unmarshal(data, &node); err != nil {
				return err
			}
			listed.Nodes = append(listed.Nodes, node)
			return nil
		}
		if header.StructType == "snapshot" || header.MessageType == "snapshot" {
			return json.Unmarshal(data, &listed.Snapshot)
		}
		return nil
	})
	if parseErr != nil {
		return LsResult{}, adapterError(Failure, -1, parseErr.Error(), "Inspect restic's tree listing.", parseErr)
	}
	if listed.Snapshot.ID == "" {
		return LsResult{}, adapterError(Failure, -1, "restic ls returned no snapshot header", "Inspect restic's tree listing.", nil)
	}
	return listed, nil
}

// Dump streams one file raw, or one directory as tar or zip, to writer. When
// it fails, writer may already contain a partial file or archive. A writer
// that blocks forever also blocks Dump, even past the process escalation
// window.
func (c *Client) Dump(ctx context.Context, options DumpOptions, writer io.Writer) error {
	if err := validateSnapshotID("dump", options.SnapshotID); err != nil {
		return err
	}
	if options.Path == "" {
		return adapterError(Failure, -1, "dump path is required", "Pass a snapshot path to restic dump.", nil)
	}
	if writer == nil {
		return adapterError(Failure, -1, "dump output is nil", "Pass the destination writer.", nil)
	}
	args := []string{"dump"}
	if options.Archive != "" {
		if options.Archive != ArchiveTar && options.Archive != ArchiveZip {
			return adapterError(Failure, -1, "unsupported dump archive "+string(options.Archive), "Use tar or zip.", nil)
		}
		args = append(args, "--archive", string(options.Archive))
	}
	args = append(args, "--", options.SnapshotID, options.Path)
	tracked := &trackingWriter{writer: writer}
	raw, runErr := c.run(ctx, invocation{
		repository: &options.Repository,
		args:       args,
		stdout:     tracked,
		retryLock:  true,
	})
	if tracked.err != nil {
		message := "write dump output: " + redactSecrets(tracked.err.Error(), raw.secrets)
		return adapterError(Failure, -1, message, "Discard the partial output and retry with a writable destination.", errors.Join(runErr, tracked.err))
	}
	return runErr
}

type trackingWriter struct {
	writer io.Writer
	err    error
}

func (w *trackingWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil && w.err == nil {
		w.err = err
	}
	return n, err
}

// Restore restores a snapshot into target, optionally limited to includes.
func (c *Client) Restore(ctx context.Context, options RestoreOptions) (RestoreSummary, error) {
	if err := validateSnapshotID("restore", options.SnapshotID); err != nil {
		return RestoreSummary{}, err
	}
	if options.Target == "" {
		return RestoreSummary{}, adapterError(Failure, -1, "restore target is required", "Pass a target directory to restic restore.", nil)
	}
	args := []string{"restore", "--target", options.Target}
	for _, include := range options.Includes {
		args = append(args, "--include", include)
	}
	args = append(args, "--", options.SnapshotID)
	raw, runErr := c.run(ctx, invocation{
		repository: &options.Repository,
		args:       args,
		retryLock:  true,
	})
	combined := append(append([]byte(nil), raw.Stdout...), raw.Stderr...)
	summary, summaryErr := restoreSummary(combined)
	if runErr != nil {
		return summary, runErr
	}
	if summaryErr != nil {
		return RestoreSummary{}, adapterError(Failure, -1, summaryErr.Error(), "Inspect restic's restore output.", summaryErr)
	}
	return summary, nil
}

// ForgetDryRunPolicy returns the snapshots restic's policy would keep and
// remove without deleting any snapshot.
func (c *Client) ForgetDryRunPolicy(ctx context.Context, options ForgetDryRunOptions) ([]ForgetGroup, error) {
	policyArgs, err := retentionArgs(options.Policy)
	if err != nil {
		return nil, err
	}
	args, err := appendSnapshotFilter([]string{"forget", "--dry-run"}, options.Filter)
	if err != nil {
		return nil, err
	}
	args = append(args, policyArgs...)
	raw, runErr := c.run(ctx, invocation{
		repository: &options.Repository,
		args:       args,
		metadata:   true,
		noLock:     true,
	})
	if runErr != nil {
		return nil, runErr
	}
	var groups []ForgetGroup
	if err := decodeJSON(raw.Stdout, &groups); err != nil {
		return nil, adapterError(Failure, -1, err.Error(), "Inspect restic's retention preview.", err)
	}
	return groups, nil
}

func retentionArgs(policy RetentionPolicy) ([]string, error) {
	values := []struct {
		name  string
		value int
	}{
		{"--keep-last", policy.KeepLast},
		{"--keep-hourly", policy.KeepHourly},
		{"--keep-daily", policy.KeepDaily},
		{"--keep-weekly", policy.KeepWeekly},
		{"--keep-monthly", policy.KeepMonthly},
		{"--keep-yearly", policy.KeepYearly},
	}
	var args []string
	for _, entry := range values {
		if entry.value < 0 {
			return nil, adapterError(Failure, -1, entry.name+" cannot be negative", "Use zero to omit a retention count.", nil)
		}
		if entry.value > 0 {
			args = append(args, entry.name, strconv.Itoa(entry.value))
		}
	}
	durations := []struct {
		name  string
		value string
	}{
		{"--keep-within", policy.KeepWithin},
		{"--keep-within-hourly", policy.KeepWithinHourly},
		{"--keep-within-daily", policy.KeepWithinDaily},
		{"--keep-within-weekly", policy.KeepWithinWeekly},
		{"--keep-within-monthly", policy.KeepWithinMonthly},
		{"--keep-within-yearly", policy.KeepWithinYearly},
	}
	for _, entry := range durations {
		if entry.value != "" {
			args = append(args, entry.name, entry.value)
		}
	}
	if len(args) == 0 {
		return nil, adapterError(Failure, -1, "retention policy is empty", "Set at least one keep count or duration.", nil)
	}
	return args, nil
}

// Forget deletes only explicit IDs. restic exits successfully for an unknown
// ID, so its redacted plain output is returned to the caller unchanged.
func (c *Client) Forget(ctx context.Context, options ForgetOptions) (TextResult, error) {
	if len(options.IDs) == 0 {
		return TextResult{}, adapterError(Failure, -1, "forget snapshot IDs are empty", "Pass one or more explicit snapshot IDs.", nil)
	}
	for _, id := range options.IDs {
		if err := validateSnapshotID("forget", id); err != nil {
			return TextResult{}, err
		}
	}
	args := []string{"forget", "--"}
	args = append(args, options.IDs...)
	raw, err := c.run(ctx, invocation{repository: &options.Repository, args: args, retryLock: true})
	output := redactedOutput(raw)
	return TextResult{Output: output}, err
}

// Prune removes unreferenced repository data.
func (c *Client) Prune(ctx context.Context, repository Repository) (TextResult, error) {
	raw, err := c.run(ctx, invocation{repository: &repository, args: []string{"prune"}, retryLock: true})
	return TextResult{Output: redactedOutput(raw)}, err
}

// Check checks repository integrity and optionally reads a subset of data.
func (c *Client) Check(ctx context.Context, options CheckOptions) (CheckSummary, error) {
	args := []string{"check"}
	if options.ReadDataSubset != "" {
		args = append(args, "--read-data-subset", options.ReadDataSubset)
	}
	raw, runErr := c.run(ctx, invocation{repository: &options.Repository, args: args, retryLock: true})
	summary, summaryErr := checkSummary(raw.Stdout)
	if runErr != nil {
		return summary, runErr
	}
	if summaryErr != nil {
		return CheckSummary{}, adapterError(Failure, -1, summaryErr.Error(), "Inspect restic's integrity-check output.", summaryErr)
	}
	return summary, nil
}

var fullSnapshotID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Copy copies exact full snapshot IDs from source to destination with the
// destination password on fd 3 and source password on fd 4. The result is
// read back from the destination so it carries the new IDs and Original IDs.
func (c *Client) Copy(ctx context.Context, options CopyOptions) (CopyResult, error) {
	if len(options.IDs) == 0 {
		return CopyResult{}, adapterError(Failure, -1, "copy snapshot IDs are empty", "Pass one or more exact snapshot IDs.", nil)
	}
	for _, id := range options.IDs {
		if !fullSnapshotID.MatchString(id) {
			return CopyResult{}, adapterError(Failure, -1, "copy requires a full 64-character snapshot ID", "Resolve prefixes before copying so restic copies exact snapshots.", nil)
		}
	}
	sourceSnapshots, err := c.snapshotsByIDs(ctx, options.Source, options.IDs)
	if err != nil {
		return CopyResult{}, err
	}
	sourceByID := make(map[string]Snapshot, len(sourceSnapshots))
	for _, snapshot := range sourceSnapshots {
		sourceByID[snapshot.ID] = snapshot
	}
	for _, id := range options.IDs {
		if _, ok := sourceByID[id]; !ok {
			return CopyResult{}, adapterError(NotFound, -1, "source snapshot not found: "+id, "Check the exact source snapshot ID.", nil)
		}
	}
	args := []string{"copy", "--from-password-file", "/dev/fd/4"}
	args = append(args, "--")
	args = append(args, options.IDs...)
	if _, err := c.run(ctx, invocation{
		repository: &options.Destination,
		source:     &options.Source,
		args:       args,
		retryLock:  true,
	}); err != nil {
		return CopyResult{}, err
	}
	snapshots, err := c.Snapshots(ctx, SnapshotsOptions{Repository: options.Destination})
	if err != nil {
		return CopyResult{}, err
	}
	copied := make([]Snapshot, 0, len(options.IDs))
	for _, id := range options.IDs {
		source := sourceByID[id]
		original := source.Original
		if original == "" {
			original = id
		}
		index := slices.IndexFunc(snapshots, func(snapshot Snapshot) bool {
			return snapshot.Original == id || snapshot.Original == original
		})
		if index < 0 {
			return CopyResult{}, adapterError(Failure, -1, "restic copy returned no destination snapshot for source "+id, "Inspect the destination for an incomplete or conflicting copy.", nil)
		}
		copied = append(copied, snapshots[index])
	}
	return CopyResult{Snapshots: copied}, nil
}

func (c *Client) snapshotsByIDs(ctx context.Context, repository Repository, ids []string) ([]Snapshot, error) {
	args := append([]string{"snapshots", "--"}, ids...)
	result, err := c.run(ctx, invocation{repository: &repository, args: args, metadata: true, noLock: true})
	if err != nil {
		return nil, err
	}
	var snapshots []Snapshot
	if err := decodeJSON(result.Stdout, &snapshots); err != nil {
		return nil, adapterError(Failure, -1, err.Error(), "Inspect restic's source snapshot list.", err)
	}
	return snapshots, nil
}

// Unlock removes stale repository locks only. It deliberately never passes
// --remove-all, so a live process's lock remains.
func (c *Client) Unlock(ctx context.Context, repository Repository) (TextResult, error) {
	raw, err := c.run(ctx, invocation{
		repository: &repository,
		args:       []string{"unlock"},
		metadata:   true,
	})
	return TextResult{Output: redactedOutput(raw)}, err
}

func validateSnapshotFilename(name string) error {
	if name == "" || path.IsAbs(name) || filepath.IsAbs(name) || strings.HasSuffix(name, "/") || strings.Contains(name, "\\") || strings.ContainsRune(name, '\x00') {
		return invalidSnapshotName(name)
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return invalidSnapshotName(name)
		}
	}
	return nil
}

func invalidSnapshotName(name string) error {
	return adapterError(Failure, -1, fmt.Sprintf("invalid stdin snapshot filename %q", name), "Use a clean relative slash-separated path without empty, dot, or parent segments.", nil)
}

func validateRelativePath(path string) error {
	if path == "" || path == "." || filepath.IsAbs(path) || filepath.Clean(path) != path {
		return invalidRelativePath(path)
	}
	for _, part := range strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' }) {
		if part == ".." {
			return invalidRelativePath(path)
		}
	}
	return nil
}

func invalidRelativePath(path string) error {
	return adapterError(Failure, -1, fmt.Sprintf("invalid backup path %q", path), "Use a clean relative path below BackupPathsOptions.BaseDir.", nil)
}

func validateSnapshotID(operation string, id string) error {
	if fullSnapshotID.MatchString(id) {
		return nil
	}
	return adapterError(Failure, -1, operation+" requires a full 64-character snapshot ID", "Resolve prefixes and aliases before calling the restic adapter.", nil)
}

func validateSnapshotMetadata(tags []string, host string) error {
	if err := validateTags(tags); err != nil {
		return err
	}
	return validateHost(host)
}

func validateTags(tags []string) error {
	for _, tag := range tags {
		if tag == "" || strings.TrimSpace(tag) != tag || strings.Contains(tag, ",") {
			return adapterError(Failure, -1, fmt.Sprintf("invalid snapshot tag %q", tag), "Use a non-empty tag without commas or surrounding whitespace.", nil)
		}
	}
	return nil
}

func validateHost(host string) error {
	if host != "" && (strings.TrimSpace(host) != host || strings.Contains(host, ",")) {
		return adapterError(Failure, -1, fmt.Sprintf("invalid snapshot host %q", host), "Use a host without commas or surrounding whitespace.", nil)
	}
	return nil
}

func redactedOutput(result CommandResult) string {
	output := strings.TrimSpace(string(append(append([]byte(nil), result.Stdout...), result.Stderr...)))
	return logctx.RedactText(redactSecrets(output, result.secrets))
}
