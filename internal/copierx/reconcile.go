package copierx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	renderStateVersion   = 1
	fingerprintRegular   = "file"
	fingerprintSymlink   = "symlink"
	fingerprintDirectory = "directory"
	fingerprintOther     = "other"
)

type ReconcileMode string

const (
	ReconcileCreate ReconcileMode = "create"
	ReconcileUpdate ReconcileMode = "update"
)

type ChangeKind string

const (
	ChangeAdd    ChangeKind = "add"
	ChangeModify ChangeKind = "modify"
	ChangeDelete ChangeKind = "delete"
	ChangeAdopt  ChangeKind = "adopt"
)

type ConflictReason string

const (
	ConflictLocallyModified    ConflictReason = "locally-modified"
	ConflictUntrackedDifferent ConflictReason = "untracked-different"
	ConflictTypeChanged        ConflictReason = "type-changed"
)

type Change struct {
	Path string     `json:"path"`
	Kind ChangeKind `json:"kind"`
}

type Conflict struct {
	Path   string         `json:"path"`
	Reason ConflictReason `json:"reason"`
}

type ReconcileResult struct {
	Changes   []Change   `json:"changes,omitempty"`
	Conflicts []Conflict `json:"conflicts,omitempty"`
}

type RenderLayer struct {
	Name     string
	Template string
	DestRoot string
	Inputs   Inputs
}

type RenderLayerState struct {
	Name        string `json:"name"`
	Template    string `json:"template"`
	DestRoot    string `json:"dest_root,omitempty"`
	AnswersFile string `json:"answers_file,omitempty"`
}

type Fingerprint struct {
	Kind   string      `json:"kind"`
	SHA256 string      `json:"sha256,omitempty"`
	Mode   fs.FileMode `json:"mode,omitempty"`
	Link   string      `json:"link,omitempty"`
}

type RenderState struct {
	Version   int                    `json:"version"`
	Layers    []RenderLayerState     `json:"layers,omitempty"`
	Files     map[string]Fingerprint `json:"files,omitempty"`
	Documents map[string][]byte      `json:"documents,omitempty"`
}

type RenderPlan struct {
	Target                string
	StatePath             string
	Layers                []RenderLayer
	Documents             []string
	AllowedSymlinkParents []string
}

type ReconcileOptions struct {
	Mode      ReconcileMode
	DryRun    bool
	Overwrite bool
}

type PreparedReconcile struct {
	plan          RenderPlan
	options       ReconcileOptions
	scratch       string
	backup        string
	metadataPaths []string
	oldState      RenderState
	newState      RenderState
	result        ReconcileResult
}

func (p *PreparedReconcile) Close() error {
	if p == nil {
		return nil
	}
	var result error
	if p.scratch != "" {
		result = os.RemoveAll(p.scratch)
	}
	if p.backup != "" {
		if err := os.RemoveAll(p.backup); result == nil {
			result = err
		}
	}
	p.scratch = ""
	p.backup = ""
	return result
}

func (p *PreparedReconcile) Result() ReconcileResult {
	if p == nil {
		return ReconcileResult{}
	}
	return p.result
}

func PrepareReconcile(ctx context.Context, plan RenderPlan, opts ReconcileOptions) (*PreparedReconcile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if plan.Target == "" {
		return nil, fmt.Errorf("render target is required")
	}
	scratch, err := os.MkdirTemp("", "angee-reconcile-*")
	if err != nil {
		return nil, fmt.Errorf("create reconciliation scratch dir: %w", err)
	}
	oldState, _, err := ReadRenderState(plan.StatePath)
	if err != nil {
		_ = os.RemoveAll(scratch)
		return nil, err
	}
	prepared := &PreparedReconcile{
		plan:     plan,
		options:  opts,
		scratch:  scratch,
		oldState: oldState,
		newState: RenderState{
			Version:   renderStateVersion,
			Files:     map[string]Fingerprint{},
			Documents: map[string][]byte{},
		},
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = prepared.Close()
		}
	}()

	metadata := map[string]struct{}{}
	for _, layer := range plan.Layers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if layer.Template == "" {
			return nil, fmt.Errorf("render layer %q: template is required", layer.Name)
		}
		dest, err := safePlanJoin(scratch, layer.DestRoot)
		if err != nil {
			return nil, fmt.Errorf("render layer %q: %w", layer.Name, err)
		}
		if err := (LocalRenderer{}).Copy(ctx, CopyRequest{Template: layer.Template, Dest: dest, Inputs: layer.Inputs}); err != nil {
			return nil, fmt.Errorf("render layer %q: %w", layer.Name, err)
		}
		cfg, err := readConfig(layer.Template)
		if err != nil {
			return nil, fmt.Errorf("render layer %q config: %w", layer.Name, err)
		}
		answerRel := filepath.ToSlash(filepath.Clean(filepath.Join(layer.DestRoot, cfg.AnswersFile)))
		metadata[answerRel] = struct{}{}
		prepared.newState.Layers = append(prepared.newState.Layers, RenderLayerState{
			Name:        layer.Name,
			Template:    layer.Template,
			DestRoot:    filepath.ToSlash(filepath.Clean(layer.DestRoot)),
			AnswersFile: answerRel,
		})
	}

	documents := map[string]struct{}{}
	for _, path := range plan.Documents {
		rel := filepath.ToSlash(filepath.Clean(path))
		if err := validateRelativePath(rel); err != nil {
			return nil, fmt.Errorf("render document: %w", err)
		}
		documents[rel] = struct{}{}
		renderedPath, err := safePlanJoin(scratch, rel)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(renderedPath)
		if err != nil {
			return nil, fmt.Errorf("read rendered document %q: %w", rel, err)
		}
		prepared.newState.Documents[rel] = data
	}
	for rel := range metadata {
		prepared.metadataPaths = append(prepared.metadataPaths, rel)
	}
	sort.Strings(prepared.metadataPaths)

	var renderedPaths []string
	err = filepath.WalkDir(scratch, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == scratch || entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(scratch, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := metadata[rel]; ok {
			return nil
		}
		if _, ok := documents[rel]; ok {
			return nil
		}
		fingerprint, exists, err := fingerprintPath(path)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		prepared.newState.Files[rel] = fingerprint
		renderedPaths = append(renderedPaths, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk rendered tree: %w", err)
	}
	paths := make(map[string]struct{}, len(renderedPaths)+len(oldState.Files))
	for _, rel := range renderedPaths {
		paths[rel] = struct{}{}
	}
	for rel := range oldState.Files {
		if err := validateRelativePath(rel); err != nil {
			return nil, fmt.Errorf("render state %q: %w", plan.StatePath, err)
		}
		paths[rel] = struct{}{}
	}
	sortedPaths := make([]string, 0, len(paths))
	for rel := range paths {
		sortedPaths = append(sortedPaths, rel)
	}
	sort.Strings(sortedPaths)
	for _, rel := range sortedPaths {
		dest, err := safePlanJoin(plan.Target, rel)
		if err != nil {
			return nil, err
		}
		current, currentExists, err := fingerprintPath(dest)
		if err != nil {
			return nil, fmt.Errorf("fingerprint current %q: %w", rel, err)
		}
		old, oldExists := oldState.Files[rel]
		newFingerprint, newExists := prepared.newState.Files[rel]
		overwrite := opts.Overwrite || opts.Mode == ReconcileCreate
		change, conflict := reconcilePath(rel, old, oldExists, current, currentExists, newFingerprint, newExists, overwrite)
		if change != nil {
			prepared.result.Changes = append(prepared.result.Changes, *change)
		}
		if conflict != nil {
			prepared.result.Conflicts = append(prepared.result.Conflicts, *conflict)
		}
	}

	cleanup = false
	return prepared, nil
}

func (p *PreparedReconcile) RenderedDocument(path string) ([]byte, bool) {
	if p == nil {
		return nil, false
	}
	data, ok := p.newState.Documents[filepath.ToSlash(filepath.Clean(path))]
	return append([]byte(nil), data...), ok
}

func (p *PreparedReconcile) PreviousDocument(path string) ([]byte, bool) {
	if p == nil {
		return nil, false
	}
	data, ok := p.oldState.Documents[filepath.ToSlash(filepath.Clean(path))]
	return append([]byte(nil), data...), ok
}

type applyJournalEntry struct {
	path      string
	backup    string
	existed   bool
	wasDelete bool
}

func (p *PreparedReconcile) ApplyFiles() (func() error, error) {
	if p == nil {
		return nil, fmt.Errorf("prepared reconciliation is nil")
	}
	if len(p.result.Conflicts) != 0 {
		return nil, fmt.Errorf("template reconciliation has %d conflict(s)", len(p.result.Conflicts))
	}
	if p.options.DryRun {
		return func() error { return nil }, nil
	}
	if p.backup != "" {
		return nil, fmt.Errorf("template reconciliation has already been applied")
	}
	backup, err := os.MkdirTemp("", "angee-reconcile-backup-*")
	if err != nil {
		return nil, fmt.Errorf("create reconciliation backup: %w", err)
	}
	p.backup = backup

	type operation struct {
		path   string
		delete bool
	}
	operations := make([]operation, 0, len(p.result.Changes)+len(p.metadataPaths))
	for _, change := range p.result.Changes {
		if change.Kind == ChangeAdopt {
			continue
		}
		operations = append(operations, operation{path: change.Path, delete: change.Kind == ChangeDelete})
	}
	for _, path := range p.metadataPaths {
		operations = append(operations, operation{path: path})
	}
	sort.SliceStable(operations, func(i, j int) bool { return operations[i].path < operations[j].path })

	journal := make([]applyJournalEntry, 0, len(operations))
	rolledBack := false
	rollback := func() error {
		if rolledBack {
			return nil
		}
		rolledBack = true
		var result error
		for i := len(journal) - 1; i >= 0; i-- {
			entry := journal[i]
			dest, joinErr := safePlanJoin(p.plan.Target, entry.path)
			if joinErr != nil {
				if result == nil {
					result = joinErr
				}
				continue
			}
			if err := os.RemoveAll(dest); err != nil && result == nil {
				result = err
			}
			if entry.existed {
				if err := copyEntry(entry.backup, dest); err != nil && result == nil {
					result = err
				}
			} else {
				removeEmptyParents(filepath.Dir(dest), p.plan.Target)
			}
		}
		return result
	}

	for index, operation := range operations {
		if err := validateDestinationParents(p.plan.Target, operation.path, p.plan.AllowedSymlinkParents); err != nil {
			_ = rollback()
			return nil, err
		}
		dest, err := safePlanJoin(p.plan.Target, operation.path)
		if err != nil {
			_ = rollback()
			return nil, err
		}
		entry := applyJournalEntry{path: operation.path, backup: filepath.Join(backup, fmt.Sprintf("%06d", index)), wasDelete: operation.delete}
		if _, exists, err := fingerprintPath(dest); err != nil {
			_ = rollback()
			return nil, fmt.Errorf("backup destination %q: %w", operation.path, err)
		} else if exists {
			entry.existed = true
			if err := copyEntry(dest, entry.backup); err != nil {
				_ = rollback()
				return nil, fmt.Errorf("backup destination %q: %w", operation.path, err)
			}
		}
		journal = append(journal, entry)

		if operation.delete {
			if err := os.RemoveAll(dest); err != nil {
				_ = rollback()
				return nil, fmt.Errorf("delete rendered path %q: %w", operation.path, err)
			}
			removeEmptyParents(filepath.Dir(dest), p.plan.Target)
			continue
		}
		source, err := safePlanJoin(p.scratch, operation.path)
		if err != nil {
			_ = rollback()
			return nil, err
		}
		if err := replaceEntry(source, dest); err != nil {
			_ = rollback()
			return nil, fmt.Errorf("install rendered path %q: %w", operation.path, err)
		}
	}
	return rollback, nil
}

func (p *PreparedReconcile) SaveState() error {
	if p == nil {
		return fmt.Errorf("prepared reconciliation is nil")
	}
	if len(p.result.Conflicts) != 0 {
		return fmt.Errorf("template reconciliation has %d conflict(s)", len(p.result.Conflicts))
	}
	if p.options.DryRun || p.plan.StatePath == "" {
		return nil
	}
	data, err := json.MarshalIndent(p.newState, "", "  ")
	if err != nil {
		return fmt.Errorf("encode render state: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(p.plan.StatePath), 0o755); err != nil {
		return fmt.Errorf("create render state directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(p.plan.StatePath), ".template-state-*")
	if err != nil {
		return fmt.Errorf("create render state temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write render state: %w", err)
	}
	if err := temp.Chmod(0o644); err != nil {
		temp.Close()
		return fmt.Errorf("chmod render state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close render state: %w", err)
	}
	if err := os.Rename(tempPath, p.plan.StatePath); err != nil {
		return fmt.Errorf("replace render state %q: %w", p.plan.StatePath, err)
	}
	return nil
}

func safePlanJoin(root, rel string) (string, error) {
	if rel == "" || rel == "." {
		return root, nil
	}
	if err := validateRelativePath(rel); err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.Clean(rel)), nil
}

func validateRelativePath(path string) error {
	if path == "" || filepath.IsAbs(path) {
		return fmt.Errorf("path %q must be a non-empty relative path", path)
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes render target", path)
	}
	return nil
}

func ReadRenderState(path string) (RenderState, bool, error) {
	empty := RenderState{
		Version:   renderStateVersion,
		Files:     map[string]Fingerprint{},
		Documents: map[string][]byte{},
	}
	if path == "" {
		return empty, false, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return empty, false, nil
	}
	if err != nil {
		return RenderState{}, false, fmt.Errorf("read render state %q: %w", path, err)
	}
	var state RenderState
	if err := json.Unmarshal(data, &state); err != nil {
		return RenderState{}, false, fmt.Errorf("decode render state %q: %w", path, err)
	}
	if state.Version != renderStateVersion {
		return RenderState{}, false, fmt.Errorf("decode render state %q: unsupported version %d", path, state.Version)
	}
	if state.Files == nil {
		state.Files = map[string]Fingerprint{}
	}
	if state.Documents == nil {
		state.Documents = map[string][]byte{}
	}
	return state, true, nil
}

func fingerprintPath(path string) (Fingerprint, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return Fingerprint{}, false, nil
	}
	if err != nil {
		return Fingerprint{}, false, err
	}
	mode := info.Mode()
	switch {
	case mode.IsRegular():
		data, err := os.ReadFile(path)
		if err != nil {
			return Fingerprint{}, false, err
		}
		sum := sha256.Sum256(data)
		return Fingerprint{Kind: fingerprintRegular, SHA256: hex.EncodeToString(sum[:]), Mode: mode.Perm()}, true, nil
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return Fingerprint{}, false, err
		}
		return Fingerprint{Kind: fingerprintSymlink, Link: target}, true, nil
	case mode.IsDir():
		return Fingerprint{Kind: fingerprintDirectory, Mode: mode.Perm()}, true, nil
	default:
		return Fingerprint{Kind: fingerprintOther, Mode: mode}, true, nil
	}
}

func reconcilePath(path string, old Fingerprint, oldExists bool, current Fingerprint, currentExists bool, newFingerprint Fingerprint, newExists bool, overwrite bool) (*Change, *Conflict) {
	if !oldExists {
		switch {
		case !newExists:
			return nil, nil
		case !currentExists:
			return &Change{Path: path, Kind: ChangeAdd}, nil
		case current == newFingerprint:
			return &Change{Path: path, Kind: ChangeAdopt}, nil
		case overwrite:
			return &Change{Path: path, Kind: ChangeModify}, nil
		default:
			reason := ConflictUntrackedDifferent
			if current.Kind != newFingerprint.Kind {
				reason = ConflictTypeChanged
			}
			return nil, &Conflict{Path: path, Reason: reason}
		}
	}

	if !newExists {
		switch {
		case !currentExists:
			return nil, nil
		case current == old || overwrite:
			return &Change{Path: path, Kind: ChangeDelete}, nil
		default:
			return nil, &Conflict{Path: path, Reason: ConflictLocallyModified}
		}
	}

	switch {
	case !currentExists:
		if overwrite {
			return &Change{Path: path, Kind: ChangeAdd}, nil
		}
		return nil, &Conflict{Path: path, Reason: ConflictLocallyModified}
	case current == newFingerprint:
		if old == newFingerprint {
			return nil, nil
		}
		return &Change{Path: path, Kind: ChangeAdopt}, nil
	case current == old:
		return &Change{Path: path, Kind: ChangeModify}, nil
	case overwrite:
		return &Change{Path: path, Kind: ChangeModify}, nil
	default:
		reason := ConflictLocallyModified
		if current.Kind != newFingerprint.Kind {
			reason = ConflictTypeChanged
		}
		return nil, &Conflict{Path: path, Reason: reason}
	}
}

func validateDestinationParents(root, rel string, allowedSymlinkParents []string) error {
	if err := validateRelativePath(rel); err != nil {
		return err
	}
	rootInfo, err := os.Lstat(root)
	if err == nil {
		if rootInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("render target %q is a symlink", root)
		}
		if !rootInfo.IsDir() {
			return fmt.Errorf("render target %q is not a directory", root)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect render target %q: %w", root, err)
	}

	parts := strings.Split(filepath.Clean(filepath.FromSlash(rel)), string(filepath.Separator))
	current := root
	allowed := map[string]struct{}{}
	for _, path := range allowedSymlinkParents {
		allowed[filepath.Clean(filepath.FromSlash(path))] = struct{}{}
	}
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect destination parent %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			relParent, relErr := filepath.Rel(root, current)
			if relErr != nil {
				return relErr
			}
			if _, ok := allowed[filepath.Clean(relParent)]; !ok {
				return fmt.Errorf("destination parent %q is a symlink", current)
			}
			continue
		}
		if !info.IsDir() {
			return fmt.Errorf("destination parent %q is not a directory", current)
		}
	}
	return nil
}

func replaceEntry(source, dest string) error {
	if _, err := os.Lstat(source); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	return copyEntry(source, dest)
}

func copyEntry(source, dest string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		return os.Symlink(target, dest)
	case info.IsDir():
		if err := os.Mkdir(dest, info.Mode().Perm()); err != nil && !os.IsExist(err) {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyEntry(filepath.Join(source, entry.Name()), filepath.Join(dest, entry.Name())); err != nil {
				return err
			}
		}
		return os.Chmod(dest, info.Mode().Perm())
	case info.Mode().IsRegular():
		input, err := os.Open(source)
		if err != nil {
			return err
		}
		defer input.Close()
		temp, err := os.CreateTemp(filepath.Dir(dest), ".angee-render-*")
		if err != nil {
			return err
		}
		tempPath := temp.Name()
		defer os.Remove(tempPath)
		if _, err := io.Copy(temp, input); err != nil {
			temp.Close()
			return err
		}
		if err := temp.Chmod(info.Mode().Perm()); err != nil {
			temp.Close()
			return err
		}
		if err := temp.Close(); err != nil {
			return err
		}
		return os.Rename(tempPath, dest)
	default:
		return fmt.Errorf("unsupported filesystem entry mode %s", info.Mode())
	}
}

func removeEmptyParents(start, stop string) {
	stop = filepath.Clean(stop)
	for current := filepath.Clean(start); current != stop && current != "."; current = filepath.Dir(current) {
		rel, err := filepath.Rel(stop, current)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return
		}
		if err := os.Remove(current); err != nil {
			return
		}
	}
}
