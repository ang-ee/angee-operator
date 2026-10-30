package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/go-playground/validator/v10"
	"gopkg.in/yaml.v3"
)

const (
	KindStack      = "stack"
	VersionCurrent = 1
)

var routeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var routeHostPattern = regexp.MustCompile(`^[a-zA-Z0-9.-]+$`)

// dnsLabelPattern is one DNS label: 1-63 letters, digits and hyphens, not
// starting or ending with a hyphen.
var dnsLabelPattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// isDNSHostName reports whether name is a host name the edge can get a
// certificate for: at most 253 characters of dot-separated DNS labels.
func isDNSHostName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !dnsLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

type Runtime string

const (
	RuntimeContainer Runtime = "container"
	RuntimeLocal     Runtime = "local"
)

type Stack struct {
	Version           int                          `yaml:"version" json:"version" validate:"oneof=1" jsonschema:"required,enum=1"`
	Kind              string                       `yaml:"kind" json:"kind" validate:"required,oneof=stack" jsonschema:"required,enum=stack"`
	Name              string                       `yaml:"name" json:"name"`
	Template          *Template                    `yaml:"template,omitempty" json:"template,omitempty"`
	Operator          Operator                     `yaml:"operator,omitempty" json:"operator,omitempty"`
	SecretsBackend    SecretsBackend               `yaml:"secrets_backend,omitempty" json:"secrets_backend,omitempty"`
	Ingress           Ingress                      `yaml:"ingress,omitempty" json:"ingress,omitempty"`
	Secrets           map[string]Secret            `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	Ports             map[string]Port              `yaml:"ports,omitempty" json:"ports,omitempty"`
	Volumes           map[string]Volume            `yaml:"volumes,omitempty" json:"volumes,omitempty"`
	Persist           map[string]PersistPath       `yaml:"persist,omitempty" json:"persist,omitempty"`
	VCS               string                       `yaml:"vcs,omitempty" json:"vcs,omitempty" validate:"omitempty,oneof=git jj" jsonschema:"enum=git,enum=jj"`
	Sources           map[string]Source            `yaml:"sources,omitempty" json:"sources,omitempty"`
	WorkspaceDefaults map[string]WorkspaceDefaults `yaml:"workspace_defaults,omitempty" json:"workspace_defaults,omitempty"`
	Workspaces        map[string]Workspace         `yaml:"workspaces,omitempty" json:"workspaces,omitempty"`
	Services          map[string]Service           `yaml:"services,omitempty" json:"services,omitempty"`
	Jobs              map[string]Job               `yaml:"jobs,omitempty" json:"jobs,omitempty"`
	PortLeases        map[string][]PortLease       `yaml:"port_leases,omitempty" json:"port_leases,omitempty"`
}

type Template struct {
	Active      string `yaml:"active,omitempty" json:"active,omitempty"`
	AnswersFile string `yaml:"answers_file,omitempty" json:"answers_file,omitempty"`
}

type Operator struct {
	URL           string              `yaml:"url,omitempty" json:"url,omitempty"`
	Domain        string              `yaml:"domain,omitempty" json:"domain,omitempty"`
	TokenSecret   string              `yaml:"token_secret,omitempty" json:"token_secret,omitempty"`
	PortPool      map[string]PortPool `yaml:"port_pool,omitempty" json:"port_pool,omitempty"`
	TemplatePaths []string            `yaml:"template_paths,omitempty" json:"template_paths,omitempty"`
}

type PortPool struct {
	Range string `yaml:"range" json:"range" validate:"required" jsonschema:"required"`
}

type PortLease struct {
	Port      int       `yaml:"port" json:"port"`
	Owner     string    `yaml:"owner" json:"owner"`
	CreatedAt time.Time `yaml:"created_at" json:"created_at"`
}

type SecretsBackend struct {
	Type    string `yaml:"type,omitempty" json:"type,omitempty" validate:"omitempty,oneof=env-file openbao" jsonschema:"enum=env-file,enum=openbao"`
	Path    string `yaml:"path,omitempty" json:"path,omitempty"`
	Address string `yaml:"address,omitempty" json:"address,omitempty"`
	Mount   string `yaml:"mount,omitempty" json:"mount,omitempty"`
	Token   string `yaml:"token,omitempty" json:"token,omitempty"`
}

// Seam: a future SecretsBackend-shaped FilesBackend block would select a
// non-localfs files store, opened via store.Open. Files default to the
// zero-config localfs backend rooted at the resolved source dir, so no
// manifest config is needed today.

type Ingress struct {
	Type    string `yaml:"type,omitempty" json:"type,omitempty" validate:"omitempty,oneof=none caddy" jsonschema:"enum=none,enum=caddy"`
	Routing string `yaml:"routing,omitempty" json:"routing,omitempty" validate:"omitempty,oneof=host path" jsonschema:"enum=host,enum=path"`
	TLS     string `yaml:"tls,omitempty" json:"tls,omitempty" validate:"omitempty,oneof=auto off" jsonschema:"enum=auto,enum=off"`
	Domain  string `yaml:"domain,omitempty" json:"domain,omitempty"`
	// Aliases are further host names the path-routing edge serves as the same
	// site as Domain, each with its own automatic certificate. Domain stays the
	// canonical host that route URLs use.
	Aliases []string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	Port    int      `yaml:"port,omitempty" json:"port,omitempty" validate:"omitempty,min=1,max=65535" jsonschema:"minimum=1,maximum=65535"`
	Image   string   `yaml:"image,omitempty" json:"image,omitempty"`
	Network string   `yaml:"network,omitempty" json:"network,omitempty"`
	Verify  string   `yaml:"verify,omitempty" json:"verify,omitempty"`
}

// RoutingMode reports the edge routing mode, defaulting to "host" when unset.
// Defaulting happens at read time (not in Defaults) so existing manifests
// re-save byte-for-byte unchanged.
func (i Ingress) RoutingMode() string {
	if i.Routing == "" {
		return "host"
	}
	return i.Routing
}

// TLSMode reports the edge TLS mode, defaulting to "auto" (Caddy automatic
// HTTPS) when unset. "off" drops the edge to plain HTTP / ws://.
func (i Ingress) TLSMode() string {
	if i.TLS == "" {
		return "auto"
	}
	return i.TLS
}

// DefaultEdgePort is the host port the plain ws:// (tls: off) edge publishes
// when ingress.port is unset. Shared by HostPort and the route-URL builder so
// the "is this the default port" decision lives in one place.
const DefaultEdgePort = 80

// HostPort reports the host port the edge publishes for the plain ws:// (tls:
// off) edge, defaulting to DefaultEdgePort when ingress.port is unset. It lets
// each stack on one host (e.g. parallel dev workspaces) bind a distinct edge
// port instead of every edge contending for :80; the consumer URL carries the
// same port. The prod (tls: auto) edge keeps the standard 443/80 and ignores
// this — ValidateExtended rejects ingress.port outside tls: off so the field is
// never a silent no-op.
func (i Ingress) HostPort() int {
	if i.Port != 0 {
		return i.Port
	}
	return DefaultEdgePort
}

type Secret struct {
	Generated bool   `yaml:"generated,omitempty" json:"generated,omitempty"`
	Length    int    `yaml:"length,omitempty" json:"length,omitempty"`
	Required  bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Import    string `yaml:"import,omitempty" json:"import,omitempty"`
}

type Port struct {
	Value     int      `yaml:"value" json:"value" validate:"gte=0" jsonschema:"minimum=0"`
	ExportEnv string   `yaml:"export_env,omitempty" json:"export_env,omitempty"`
	Aliases   []string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
}

type Volume struct {
	Driver string `yaml:"driver,omitempty" json:"driver,omitempty"`
	Path   string `yaml:"path,omitempty" json:"path,omitempty"`
}

type Source struct {
	Kind string `yaml:"kind" json:"kind" validate:"required,oneof=git local" jsonschema:"required,enum=git,enum=local"`
	// Repo is the one-remote form: it declares the remote `origin`. Remotes
	// declares several; a source sets one or the other.
	Repo    string                  `yaml:"repo,omitempty" json:"repo,omitempty"`
	Remotes map[string]SourceRemote `yaml:"remotes,omitempty" json:"remotes,omitempty"`
	// PushDefault names the declared remote a push goes to when nothing more
	// specific (a branch's pushRemote) says otherwise. It becomes the cache's
	// remote.pushDefault.
	PushDefault string     `yaml:"push_default,omitempty" json:"push_default,omitempty"`
	URL         string     `yaml:"url,omitempty" json:"url,omitempty"`
	Path        string     `yaml:"path,omitempty" json:"path,omitempty"`
	DefaultRef  string     `yaml:"default_ref,omitempty" json:"default_ref,omitempty"`
	CachePath   string     `yaml:"cache_path,omitempty" json:"cache_path,omitempty"`
	Auth        SourceAuth `yaml:"auth,omitempty" json:"auth,omitempty"`
	Git         SourceGit  `yaml:"git,omitempty" json:"git,omitempty"`
	Checksum    string     `yaml:"checksum,omitempty" json:"checksum,omitempty"`
}

// DefaultRemote is the remote a source's `repo:` declares.
const DefaultRemote = "origin"

// SourceRemote is one remote of a git source. In YAML and JSON a bare string
// is the URL alone: `origin: git@github.com:me/app.git`.
type SourceRemote struct {
	URL string `yaml:"url" json:"url"`
	// Canonical marks the fetch-only remote: the cache is cloned from it and
	// can never push to it.
	Canonical bool `yaml:"canonical,omitempty" json:"canonical,omitempty"`
	// Branches lists the branch patterns (path.Match syntax) a publish to this
	// remote may create or update. Empty allows any branch.
	Branches []string `yaml:"branches,omitempty" json:"branches,omitempty"`
}

// sourceRemoteFields are the keys the mapping form of a SourceRemote accepts.
// A custom UnmarshalYAML decodes with a fresh decoder, which does not inherit
// LoadFile's KnownFields, so the mapping form checks its keys itself.
var sourceRemoteFields = map[string]bool{"url": true, "canonical": true, "branches": true}

func (r *SourceRemote) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		*r = SourceRemote{URL: value.Value}
		return nil
	}
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a remote is a URL or a mapping with url, canonical and branches", value.Line)
	}
	if err := checkSourceRemoteKeys(value); err != nil {
		return err
	}
	type plain SourceRemote
	var decoded plain
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	*r = SourceRemote(decoded)
	return nil
}

// checkSourceRemoteKeys refuses a key the mapping form does not have. A `<<`
// merge key is accepted, as everywhere else in the manifest, and the mappings
// it merges are checked the same way.
func checkSourceRemoteKeys(mapping *yaml.Node) error {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i], mapping.Content[i+1]
		if key.Kind == yaml.ScalarNode && key.Value == "<<" && (key.Tag == "!!merge" || key.Tag == "") {
			merged := []*yaml.Node{value}
			if value.Kind == yaml.SequenceNode {
				merged = value.Content
			}
			for _, node := range merged {
				if node.Kind == yaml.AliasNode {
					node = node.Alias
				}
				if node == nil || node.Kind != yaml.MappingNode {
					return fmt.Errorf("line %d: a merge key in a remote takes a mapping", key.Line)
				}
				if err := checkSourceRemoteKeys(node); err != nil {
					return err
				}
			}
			continue
		}
		if !sourceRemoteFields[key.Value] {
			return fmt.Errorf("line %d: field %s not found in a remote (url, canonical, branches)", key.Line, key.Value)
		}
	}
	return nil
}

// MarshalYAML writes a remote that has only a URL in the bare-string form, so
// a manifest the operator saves keeps the form it was written in.
func (r SourceRemote) MarshalYAML() (any, error) {
	if !r.Canonical && len(r.Branches) == 0 {
		return r.URL, nil
	}
	type plain SourceRemote
	return plain(r), nil
}

func (r *SourceRemote) UnmarshalJSON(data []byte) error {
	var url string
	if err := json.Unmarshal(data, &url); err == nil {
		*r = SourceRemote{URL: url}
		return nil
	}
	type plain SourceRemote
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var decoded plain
	if err := dec.Decode(&decoded); err != nil {
		return err
	}
	*r = SourceRemote(decoded)
	return nil
}

// DeclaredRemotes returns the remotes the source declares: `remotes:` as
// written, or `repo:` as the one remote origin. It is nil when neither is set.
func (s Source) DeclaredRemotes() map[string]SourceRemote {
	if len(s.Remotes) > 0 {
		return s.Remotes
	}
	if s.Repo != "" {
		return map[string]SourceRemote{DefaultRemote: {URL: s.Repo}}
	}
	return nil
}

// CloneRemote names the remote a new cache is cloned from: the canonical
// remote, else origin, else the only remote. ok is false when the source
// declares no remote, or several of which none qualifies (ValidateSource
// refuses the latter).
func (s Source) CloneRemote() (name string, remote SourceRemote, ok bool) {
	remotes := s.DeclaredRemotes()
	for name, remote := range remotes {
		if remote.Canonical {
			return name, remote, true
		}
	}
	if remote, ok := remotes[DefaultRemote]; ok {
		return DefaultRemote, remote, true
	}
	if len(remotes) == 1 {
		for name, remote := range remotes {
			return name, remote, true
		}
	}
	return "", SourceRemote{}, false
}

// CloneURL returns the URL a new cache is cloned from (CloneRemote), or "" when
// the source declares no remote.
func (s Source) CloneURL() string {
	_, remote, _ := s.CloneRemote()
	return remote.URL
}

// IsRelativeURL reports whether a remote URL is a relative filesystem path
// rather than a URL, an scp-like `host:path`, or an absolute path. git records
// a clone's source as an absolute path and resolves a relative remote URL
// against the checkout, so a relative URL means something else in each one.
func IsRelativeURL(url string) bool {
	if strings.Contains(url, "://") || filepath.IsAbs(url) {
		return false
	}
	colon := strings.Index(url, ":")
	slash := strings.Index(url, "/")
	scpLike := colon > 0 && (slash < 0 || colon < slash)
	return !scpLike
}

// remoteNamePattern is a conservative subset of git's remote names: it keeps
// every name usable as a config section and a ref path component.
var remoteNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// checkRemoteURL refuses a URL git would store differently from what the
// manifest says: one with a control character (a newline would become a second
// URL line in the git config) or with surrounding spaces.
func checkRemoteURL(url string) error {
	if strings.TrimSpace(url) != url {
		return errors.New("has leading or trailing spaces")
	}
	if strings.ContainsFunc(url, unicode.IsControl) {
		return errors.New("contains a control character")
	}
	return nil
}

// ValidateSource checks a source's remotes:
//   - `repo:` and `remotes:` are not both set;
//   - every URL, `repo:` included, has no control character or surrounding
//     spaces;
//   - every remote under `remotes:` has a valid name that differs from the
//     others by more than case, and an absolute or remote URL;
//   - at most one is canonical, and a canonical remote has no branches;
//   - push_default names a declared remote that is not canonical;
//   - the remote to clone from can be determined (CloneRemote).
func ValidateSource(name string, s Source) error {
	if s.Repo != "" && len(s.Remotes) > 0 {
		return fmt.Errorf("source %q: set repo or remotes, not both (repo declares the remote %s)", name, DefaultRemote)
	}
	if (len(s.Remotes) > 0 || s.PushDefault != "") && s.Kind != "git" {
		return fmt.Errorf("source %q: remotes and push_default require kind: git", name)
	}
	if err := checkRemoteURL(s.Repo); err != nil {
		return fmt.Errorf("source %q: repo %w", name, err)
	}
	canonical := ""
	folded := map[string]string{}
	for _, remoteName := range slices.Sorted(maps.Keys(s.Remotes)) {
		remote := s.Remotes[remoteName]
		if !remoteNamePattern.MatchString(remoteName) || strings.HasSuffix(remoteName, ".lock") || strings.Contains(remoteName, "..") {
			return fmt.Errorf("source %q: remote name %q must start with a letter or digit and hold only letters, digits, '.', '_' and '-'", name, remoteName)
		}
		// Remote-tracking refs live in files, so on a case-insensitive file
		// system two such remotes would share refs/remotes/<name>.
		if other, ok := folded[strings.ToLower(remoteName)]; ok {
			return fmt.Errorf("source %q: remote names %q and %q differ only in case", name, other, remoteName)
		}
		folded[strings.ToLower(remoteName)] = remoteName
		if remote.URL == "" {
			return fmt.Errorf("source %q remote %q: url is required", name, remoteName)
		}
		if err := checkRemoteURL(remote.URL); err != nil {
			return fmt.Errorf("source %q remote %q: url %w", name, remoteName, err)
		}
		if IsRelativeURL(remote.URL) {
			return fmt.Errorf("source %q remote %q: url %q is a relative path; use an absolute path or a URL", name, remoteName, remote.URL)
		}
		if remote.Canonical {
			if canonical != "" {
				return fmt.Errorf("source %q: remotes %q and %q are both canonical; mark one", name, canonical, remoteName)
			}
			canonical = remoteName
			if len(remote.Branches) > 0 {
				return fmt.Errorf("source %q remote %q: a canonical remote is fetch-only, so branches has nothing to allow", name, remoteName)
			}
		}
		for _, pattern := range remote.Branches {
			if _, err := path.Match(pattern, ""); err != nil || pattern == "" {
				return fmt.Errorf("source %q remote %q: branches pattern %q is not a valid path.Match pattern", name, remoteName, pattern)
			}
		}
	}
	remotes := s.DeclaredRemotes()
	if s.PushDefault != "" {
		remote, ok := remotes[s.PushDefault]
		if !ok {
			return fmt.Errorf("source %q: push_default %q names no declared remote", name, s.PushDefault)
		}
		if remote.Canonical {
			return fmt.Errorf("source %q: push_default %q is the canonical remote, which is fetch-only", name, s.PushDefault)
		}
	}
	if len(remotes) > 0 {
		if _, _, ok := s.CloneRemote(); !ok {
			return fmt.Errorf("source %q: cannot tell which remote to clone from; mark one canonical or name one %s", name, DefaultRemote)
		}
	}
	return nil
}

type SourceAuth struct {
	Mode         string `yaml:"mode,omitempty" json:"mode,omitempty"`
	SSHKeySecret string `yaml:"ssh_key_secret,omitempty" json:"ssh_key_secret,omitempty"`
	TokenSecret  string `yaml:"token_secret,omitempty" json:"token_secret,omitempty"`
}

type SourceGit struct {
	UserName  string `yaml:"user_name,omitempty" json:"user_name,omitempty"`
	UserEmail string `yaml:"user_email,omitempty" json:"user_email,omitempty"`
}

// WorkspaceDefaults carries the stack's input defaults for every workspace cut
// from one template. The map is keyed by template ref the way
// `angee workspace create --template` resolves it (`workspaces/src`; the bare
// name `src` is accepted). Inputs layer lowest to highest: the template's
// `_angee.inputs` defaults, these defaults, a declared workspace's `inputs`,
// then the request's `--input` values.
type WorkspaceDefaults struct {
	Inputs map[string]string `yaml:"inputs,omitempty" json:"inputs,omitempty"`
}

type Workspace struct {
	Template     string                     `yaml:"template" json:"template" validate:"required" jsonschema:"required"`
	Inputs       map[string]string          `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Sources      map[string]WorkspaceSource `yaml:"sources,omitempty" json:"sources,omitempty"`
	Resolved     WorkspaceResolved          `yaml:"resolved,omitempty" json:"resolved,omitempty"`
	TTL          string                     `yaml:"ttl,omitempty" json:"ttl,omitempty"`
	TTLExpiresAt *time.Time                 `yaml:"ttl_expires_at,omitempty" json:"ttl_expires_at,omitempty"`
}

type WorkspaceSource struct {
	Source string `yaml:"source" json:"source" validate:"required" jsonschema:"required"`
	// Mode selects how a source is checked out: a worktree of a git source's
	// shared cache, a standalone clone, or a jj workspace of the cache, or of
	// a local source that already is a jj repository. Omitted, the stack's vcs
	// decides (Stack.EffectiveSourceMode). A local source is linked under any
	// mode but jj.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty" jsonschema:"enum=worktree,enum=clone,enum=jj"`
	// VCS, for a slot that names no mode, is the version control the slot
	// was checked out with: the stack's vcs when angee cut it, which create
	// records, so that a later change of the stack's vcs does not read an
	// existing slot as another kind of checkout than the one on disk (a jj
	// workspace read as a git clone leaks its jj name on destroy). Omitted,
	// as on a slot cut before angee recorded it, the stack's vcs decides
	// (Stack.EffectiveSourceMode). A slot that names a mode ignores it.
	VCS string `yaml:"vcs,omitempty" json:"vcs,omitempty" jsonschema:"enum=git,enum=jj"`
	// Branch is the branch a worktree slot is cut on. Omitted, the worktree
	// starts on a detached HEAD at Ref and gets a branch when it is published.
	Branch  string `yaml:"branch,omitempty" json:"branch,omitempty"`
	Ref     string `yaml:"ref,omitempty" json:"ref,omitempty"`
	Subpath string `yaml:"subpath,omitempty" json:"subpath,omitempty"`
}

// Workspace source modes. The empty mode is also valid and leaves the choice
// to the stack's vcs (Stack.EffectiveSourceMode).
const (
	WorkspaceSourceModeWorktree = "worktree"
	WorkspaceSourceModeClone    = "clone"
	WorkspaceSourceModeJJ       = "jj"
)

// The version control systems a stack's vcs can name: the one a workspace
// slot that names no mode is checked out with (Stack.EffectiveSourceMode).
// Empty is git.
const (
	VCSGit = "git"
	VCSJJ  = "jj"
)

// ValidateWorkspaceSourceMode rejects a workspace source mode the operator
// does not implement, so a typo fails loudly instead of quietly cloning.
func ValidateWorkspaceSourceMode(mode string) error {
	switch mode {
	case "", WorkspaceSourceModeWorktree, WorkspaceSourceModeClone, WorkspaceSourceModeJJ:
		return nil
	}
	return fmt.Errorf("mode %q is not supported; use %q, %q or %q, or omit it", mode, WorkspaceSourceModeWorktree, WorkspaceSourceModeClone, WorkspaceSourceModeJJ)
}

// EffectiveSourceMode is the mode ws is checked out in, which everything that
// reads a slot's mode asks. A mode the slot names wins. With none, the vcs
// the slot was cut with (WorkspaceSource.VCS), else the stack's, decides: jj
// makes the slot a jj workspace, and git leaves the mode empty, keeping each
// source kind's default: a git source is cloned and a local source linked
// (plan D3). The manifest keeps the mode as written, so the stack's vcs
// decides only for a slot that names no mode and records no vcs, as one cut
// before create recorded it.
func (s *Stack) EffectiveSourceMode(ws WorkspaceSource) string {
	vcs := ws.VCS
	if vcs == "" && s != nil {
		vcs = s.VCS
	}
	switch {
	case ws.Mode != "":
		return ws.Mode
	case vcs == VCSJJ:
		return WorkspaceSourceModeJJ
	}
	return ""
}

// SlotVCS is the vcs create records on a slot that names no mode
// (WorkspaceSource.VCS): the stack's, git when it names none.
func (s *Stack) SlotVCS() string {
	if s != nil && s.VCS == VCSJJ {
		return VCSJJ
	}
	return VCSGit
}

// ValidateWorkspaceSourceVCS rejects a slot vcs the operator does not know.
func ValidateWorkspaceSourceVCS(vcs string) error {
	switch vcs {
	case "", VCSGit, VCSJJ:
		return nil
	}
	return fmt.Errorf("vcs %q is not supported; use %q or %q, or omit it", vcs, VCSGit, VCSJJ)
}

type WorkspaceResolved struct {
	Chain        []string               `yaml:"chain,omitempty" json:"chain,omitempty"`
	ChainRoot    string                 `yaml:"chain_root,omitempty" json:"chain_root,omitempty"`
	Allocations  map[string]int         `yaml:"allocations,omitempty" json:"allocations,omitempty"`
	PersistPaths map[string]PersistPath `yaml:"persist_paths,omitempty" json:"persist_paths,omitempty"`

	// LegacyLifecycle accepts the `lifecycle:` field that older
	// manifests (pre-workspace-pure-file-primitive refactor in commit
	// f48784c) persisted under workspaces[*].resolved. The strict YAML
	// decoder would otherwise reject the file on load. The value is
	// ignored and never re-emitted (no json tag, `omitempty`), so a
	// `stack update` round-trip silently drops it from the file.
	LegacyLifecycle string `yaml:"lifecycle,omitempty" json:"-"`
}

type PersistPath struct {
	// Backup excludes physical database state or other unsafe file-copy resources.
	Backup  string `yaml:"backup,omitempty" json:"backup,omitempty" validate:"omitempty,oneof=exclude" jsonschema:"enum=exclude"`
	Subpath string `yaml:"subpath" json:"subpath"`
	Scope   string `yaml:"scope" json:"scope"`
}

type Service struct {
	Runtime   Runtime           `yaml:"runtime" json:"runtime" validate:"required,oneof=container local" jsonschema:"required,enum=container,enum=local"`
	Image     string            `yaml:"image,omitempty" json:"image,omitempty"`
	Build     any               `yaml:"build,omitempty" json:"build,omitempty"`
	Command   []string          `yaml:"command,omitempty" json:"command,omitempty"`
	Env       map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	EnvFile   string            `yaml:"env_file,omitempty" json:"env_file,omitempty"`
	Ports     StringList        `yaml:"ports,omitempty" json:"ports,omitempty"`
	Mounts    StringList        `yaml:"mounts,omitempty" json:"mounts,omitempty"`
	Workdir   string            `yaml:"workdir,omitempty" json:"workdir,omitempty"`
	After     []string          `yaml:"after,omitempty" json:"after,omitempty"`
	DependsOn []string          `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
	Route     *Route            `yaml:"route,omitempty" json:"route,omitempty"`
	Ready     *ReadyProbe       `yaml:"ready,omitempty" json:"ready,omitempty"`
	// StopGracePeriod bounds graceful shutdown before the runtime force-stops
	// the service. It uses Go duration syntax (for example, "30s").
	StopGracePeriod string `yaml:"stop_grace_period,omitempty" json:"stop_grace_period,omitempty" jsonschema:"default=10s"`
	// Restart is the restart policy the runtime applies when the service
	// exits: on_failure (the default) after a non-zero exit, always after any
	// exit, no never. A stop the runtime was asked for (stack down, service
	// stop or restart) never triggers one.
	Restart RestartPolicy `yaml:"restart,omitempty" json:"restart,omitempty" validate:"omitempty,oneof=no on_failure always" jsonschema:"enum=no,enum=on_failure,enum=always,default=on_failure"`
}

const DefaultStopGracePeriod = "10s"

func (s Service) NormalizedStopGracePeriod() string {
	if s.StopGracePeriod == "" {
		return DefaultStopGracePeriod
	}
	return s.StopGracePeriod
}

// RestartPolicy is a service's restart policy. Each runtime backend compiles
// it into its own policy; the operator never restarts a process itself.
type RestartPolicy string

const (
	// RestartNo leaves a service stopped after it exits.
	RestartNo RestartPolicy = "no"
	// RestartOnFailure restarts a service after a non-zero exit, a crash or a
	// kill from outside the runtime.
	RestartOnFailure RestartPolicy = "on_failure"
	// RestartAlways restarts a service after any exit, a clean one included.
	RestartAlways RestartPolicy = "always"

	// DefaultRestartPolicy applies to a service that sets no restart.
	DefaultRestartPolicy = RestartOnFailure
)

// NormalizedRestart returns the service's restart policy, or
// DefaultRestartPolicy when it sets none. It never mutates the manifest, so an
// omitted restart stays omitted when the manifest is saved again.
func (s Service) NormalizedRestart() RestartPolicy {
	if s.Restart == "" {
		return DefaultRestartPolicy
	}
	return s.Restart
}

type Route struct {
	Port int    `yaml:"port" json:"port" validate:"required,gte=1,lte=65535" jsonschema:"required,minimum=1,maximum=65535"`
	Host string `yaml:"host,omitempty" json:"host,omitempty"`
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	Auth string `yaml:"auth,omitempty" json:"auth,omitempty" validate:"omitempty,oneof=forward none" jsonschema:"enum=forward,enum=none"`
}

type ReadyProbe struct {
	HTTP        *ReadyHTTP `yaml:"http,omitempty" json:"http,omitempty" jsonschema:"oneof_required=http"`
	TCP         *ReadyTCP  `yaml:"tcp,omitempty" json:"tcp,omitempty" jsonschema:"oneof_required=tcp"`
	Cmd         []string   `yaml:"cmd,omitempty" json:"cmd,omitempty" jsonschema:"oneof_required=cmd,minItems=1"`
	File        string     `yaml:"file,omitempty" json:"file,omitempty" jsonschema:"oneof_required=file,minLength=1"`
	Interval    string     `yaml:"interval,omitempty" json:"interval,omitempty" jsonschema:"default=5s"`
	Timeout     string     `yaml:"timeout,omitempty" json:"timeout,omitempty" jsonschema:"default=3s"`
	Retries     *int       `yaml:"retries,omitempty" json:"retries,omitempty" validate:"omitempty,gte=1" jsonschema:"minimum=1,default=12"`
	StartPeriod string     `yaml:"start_period,omitempty" json:"start_period,omitempty" jsonschema:"default=0s"`
}

type ReadyHTTP struct {
	Port int    `yaml:"port" json:"port" validate:"required,gte=1,lte=65535" jsonschema:"required,minimum=1,maximum=65535"`
	Path string `yaml:"path,omitempty" json:"path,omitempty" jsonschema:"default=/"`
}

type ReadyTCP struct {
	Port int `yaml:"port" json:"port" validate:"required,gte=1,lte=65535" jsonschema:"required,minimum=1,maximum=65535"`
}

// Normalized returns a copy of the readiness probe with documented defaults
// applied. It never mutates the manifest, so omitted defaults stay omitted
// when an existing manifest is saved again.
func (p ReadyProbe) Normalized() ReadyProbe {
	normalized := p
	if normalized.HTTP != nil {
		httpProbe := *normalized.HTTP
		if httpProbe.Path == "" {
			httpProbe.Path = "/"
		} else if !strings.HasPrefix(httpProbe.Path, "/") {
			httpProbe.Path = "/" + httpProbe.Path
		}
		normalized.HTTP = &httpProbe
	}
	if normalized.Interval == "" {
		normalized.Interval = "5s"
	}
	if normalized.Timeout == "" {
		normalized.Timeout = "3s"
	}
	if normalized.Retries == nil {
		defaultRetries := 12
		normalized.Retries = &defaultRetries
	}
	if normalized.StartPeriod == "" {
		normalized.StartPeriod = "0s"
	}
	return normalized
}

// PathPrefix returns the normalized public prefix for path routing: a single
// leading slash and no trailing slash (e.g. "/agent"). An explicit path that
// trims to empty (such as "/") denotes the ingress root and returns "". An
// unset path still defaults to "/<serviceName>".
func (r *Route) PathPrefix(serviceName string) string {
	if r.Path == "" {
		return "/" + strings.Trim(serviceName, "/")
	}
	p := strings.Trim(r.Path, "/")
	if p == "" {
		return ""
	}
	return "/" + p
}

// HostName returns the host for host-mode routing: route.host when set,
// otherwise "<serviceName>.<domain>" ("<serviceName>" when domain is empty).
// Shared by the URL resolver and the edge label generator so the URL a consumer
// sees and the host the edge actually matches on cannot drift.
func (r *Route) HostName(serviceName, domain string) string {
	if r.Host != "" {
		return r.Host
	}
	if domain != "" {
		return serviceName + "." + domain
	}
	return serviceName
}

// BackupCapture declares a trusted runtime job that emits a logical capture.
// The command receives operator-selected paths through ${inputs.backup_output}
// (stack-relative) and ${inputs.backup_output_absolute} (local host absolute).
// Output names one regular file under that directory. No commands come from API requests.
type BackupCapture struct {
	Group          string   `yaml:"group" json:"group" validate:"oneof=databases service_state" jsonschema:"enum=databases,enum=service_state"`
	Name           string   `yaml:"name" json:"name" validate:"required"`
	Output         string   `yaml:"output" json:"output" validate:"required"`
	ExcludePersist []string `yaml:"exclude_persist,omitempty" json:"exclude_persist,omitempty"`
}

type Job struct {
	Capture *BackupCapture `yaml:"capture,omitempty" json:"capture,omitempty"`

	Runtime   Runtime           `yaml:"runtime" json:"runtime" validate:"required,oneof=container local" jsonschema:"required,enum=container,enum=local"`
	Image     string            `yaml:"image,omitempty" json:"image,omitempty"`
	Build     any               `yaml:"build,omitempty" json:"build,omitempty"`
	Command   []string          `yaml:"command,omitempty" json:"command,omitempty"`
	Env       map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	EnvFile   string            `yaml:"env_file,omitempty" json:"env_file,omitempty"`
	Mounts    StringList        `yaml:"mounts,omitempty" json:"mounts,omitempty"`
	Workdir   string            `yaml:"workdir,omitempty" json:"workdir,omitempty"`
	DependsOn []string          `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
	RunOn     []string          `yaml:"run_on,omitempty" json:"run_on,omitempty"`
}

type StringList []string

func (s *StringList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		*s = StringList{value.Value}
		return nil
	}
	if value.Kind != yaml.SequenceNode {
		return fmt.Errorf("expected string list")
	}
	items := make([]string, 0, len(value.Content))
	for _, item := range value.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			items = append(items, item.Value)
		case yaml.MappingNode:
			items = append(items, stringifyMapping(item))
		default:
			return fmt.Errorf("unsupported list item kind %d", item.Kind)
		}
	}
	*s = items
	return nil
}

func stringifyMapping(node *yaml.Node) string {
	values := map[string]string{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		values[node.Content[i].Value] = node.Content[i+1].Value
	}
	if typ, ok := values["type"]; ok {
		source := values["source"]
		target := values["target"]
		ro := ""
		if values["read_only"] == "true" || values["ro"] == "true" {
			ro = ":ro"
		}
		switch typ {
		case "volume":
			return "volume://" + source + ":" + target + ro
		case "bind":
			return "bind://" + source + ":" + target + ro
		}
	}
	if port, ok := values["port"]; ok {
		if host := values["host"]; host != "" {
			return host + ":" + port
		}
		return port
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values[key])
	}
	return strings.Join(parts, ",")
}

func LoadFile(path string) (*Stack, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var stack Stack
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&stack); err != nil {
		return nil, err
	}
	// Drop legacy compat fields so SaveFile doesn't re-emit them.
	// LegacyLifecycle is the only such field today; if more appear,
	// keep them clustered here.
	for name, ws := range stack.Workspaces {
		if ws.Resolved.LegacyLifecycle != "" {
			ws.Resolved.LegacyLifecycle = ""
			stack.Workspaces[name] = ws
		}
	}
	stack.Defaults()
	if err := stack.Validate(); err != nil {
		return nil, err
	}
	return &stack, nil
}

func SaveFile(path string, stack *Stack) error {
	data, err := Marshal(stack)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Marshal applies Defaults and Validate, then renders the manifest to the
// canonical YAML bytes SaveFile would write — without writing. Useful for
// computing whether a transform changed the manifest (e.g. a template
// re-render) and for dry-run diffs.
func Marshal(stack *Stack) ([]byte, error) {
	if stack == nil {
		return nil, errors.New("manifest is nil")
	}
	stack.Defaults()
	if err := stack.Validate(); err != nil {
		return nil, err
	}
	return yaml.Marshal(stack)
}

func Path(root string) string {
	return filepath.Join(root, "angee.yaml")
}

func ResolvePath(root, p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(root, p))
}

func (s *Stack) EnvFilePath(root string) string {
	path := s.SecretsBackend.Path
	if path == "" {
		path = ".env"
	}
	return ResolvePath(root, path)
}

func (s *Stack) Defaults() {
	if s.Version == 0 {
		s.Version = VersionCurrent
	}
	if s.Kind == "" {
		s.Kind = KindStack
	}
	if s.SecretsBackend.Type == "" {
		s.SecretsBackend.Type = "env-file"
	}
	if s.Ingress.Type == "" {
		s.Ingress.Type = "none"
	}
	s.initMaps()
}

func (s *Stack) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("manifest name is required")
	}
	if err := s.validateServiceRuntime(); err != nil {
		return err
	}
	if err := validateStruct(s); err != nil {
		return err
	}
	return s.ValidateExtended()
}

func validateStruct(stack *Stack) error {
	v := validator.New()
	if err := v.Struct(stack); err != nil {
		return fmt.Errorf("manifest validation: %w", err)
	}
	return nil
}

// hasCaddyMeta reports whether s contains characters that could break out of
// a caddy-docker-proxy label into adjacent Caddyfile directives.
func hasCaddyMeta(s string) bool { return strings.ContainsAny(s, " \t\r\n{}#\"`") }

func (s *Stack) ValidateExtended() error {
	if err := s.validateDependencyGraph(); err != nil {
		return err
	}
	for name, service := range s.Services {
		if service.Route != nil && service.Runtime == RuntimeLocal {
			return fmt.Errorf("service %q: route requires runtime: container", name)
		}
		if err := validateRunnable("service", name, service.Runtime, service.Image, service.Build, service.Command); err != nil {
			return err
		}
	}
	// ingress.port only has meaning for the plain-HTTP (tls: off) Caddy edge.
	// Reject it elsewhere rather than silently ignoring it, matching how the
	// mode-mismatched route.host/route.path overrides below are handled.
	if s.Ingress.Port != 0 {
		if s.Ingress.Type != "caddy" {
			return errors.New("ingress.port requires ingress.type: caddy")
		}
		if s.Ingress.TLSMode() != "off" {
			return errors.New("ingress.port requires ingress.tls: off (the tls: auto edge uses 443/80)")
		}
	}
	if len(s.Ingress.Aliases) > 0 {
		if s.Ingress.Type != "caddy" {
			return errors.New("ingress.aliases requires ingress.type: caddy")
		}
		if s.Ingress.RoutingMode() != "path" {
			return errors.New("ingress.aliases requires ingress.routing: path")
		}
		// The edge serves the ingress domain, or the operator's when unset.
		domain := s.Ingress.Domain
		if domain == "" {
			domain = s.Operator.Domain
		}
		seen := map[string]bool{strings.ToLower(domain): true}
		for _, alias := range s.Ingress.Aliases {
			if !isDNSHostName(alias) {
				return fmt.Errorf("ingress.aliases: %q is not a host name", alias)
			}
			// Host names are case-insensitive.
			key := strings.ToLower(alias)
			if seen[key] {
				return fmt.Errorf("ingress.aliases: %q repeats the domain or another alias", alias)
			}
			seen[key] = true
		}
	}
	if s.Ingress.Type == "caddy" {
		fields := []struct {
			name  string
			value string
		}{
			{name: "domain", value: s.Ingress.Domain},
			{name: "image", value: s.Ingress.Image},
			{name: "network", value: s.Ingress.Network},
			{name: "verify", value: s.Ingress.Verify},
		}
		for _, field := range fields {
			if hasCaddyMeta(field.value) {
				return fmt.Errorf("ingress.%s contains invalid characters", field.name)
			}
		}
		if s.Ingress.RoutingMode() == "path" && s.Ingress.Domain == "" && s.Operator.Domain == "" {
			return errors.New("ingress.routing: path requires ingress.domain or operator.domain")
		}
		rootRouteFound := false
		for name, service := range s.Services {
			if service.Route == nil {
				continue
			}
			if name == "edge" {
				return fmt.Errorf("service %q: name is reserved for the ingress edge", name)
			}
			if !routeNamePattern.MatchString(name) {
				return fmt.Errorf("service %q: name not allowed for ingress routing (must match %s)", name, routeNamePattern.String())
			}
			if service.Route.Host != "" && service.Route.Path != "" {
				return fmt.Errorf("service %q: route.host and route.path are mutually exclusive", name)
			}
			// Reject the override that the active routing mode ignores, so a
			// mode-mismatched field surfaces as an error instead of a silent
			// no-op.
			if s.Ingress.RoutingMode() == "host" && service.Route.Path != "" {
				return fmt.Errorf("service %q: route.path requires ingress.routing: path", name)
			}
			if s.Ingress.RoutingMode() == "path" && service.Route.Host != "" {
				return fmt.Errorf("service %q: route.host requires ingress.routing: host", name)
			}
			if s.Ingress.RoutingMode() == "path" && service.Route.PathPrefix(name) == "" {
				if rootRouteFound {
					return errors.New("multiple services route the ingress root")
				}
				rootRouteFound = true
			}
			if service.Route.Host != "" && !routeHostPattern.MatchString(service.Route.Host) {
				return fmt.Errorf("service %q: route.host %q contains invalid characters", name, service.Route.Host)
			}
			if hasCaddyMeta(service.Route.Path) {
				return fmt.Errorf("service %q: route.path %q contains invalid characters", name, service.Route.Path)
			}
		}
	}
	captures := map[string]bool{}
	for name, persist := range s.Persist {
		if persist.Backup != "" && persist.Backup != "exclude" {
			return fmt.Errorf("persist %q backup must be exclude or omitted", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.Jobs)) {
		job := s.Jobs[name]
		if err := validateRunnable("job", name, job.Runtime, job.Image, job.Build, job.Command); err != nil {
			return err
		}
		if capture := job.Capture; capture != nil {
			if len(job.RunOn) > 0 {
				return fmt.Errorf("capture job %q is manual and cannot declare run_on", name)
			}
			if !validCaptureName(name) || !validCaptureName(capture.Name) || !validCaptureName(capture.Output) || (capture.Group != "databases" && capture.Group != "service_state") {
				return fmt.Errorf("invalid capture declaration on job %q", name)
			}
			key := capture.Group + ":" + capture.Name
			if captures[key] {
				return fmt.Errorf("duplicate backup capture %q", key)
			}
			captures[key] = true
			for _, persist := range capture.ExcludePersist {
				if _, ok := s.Persist[persist]; !ok {
					return fmt.Errorf("capture job %q excludes undeclared persist %q", name, persist)
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.Sources)) {
		if err := ValidateSource(name, s.Sources[name]); err != nil {
			return err
		}
	}
	for name, workspace := range s.Workspaces {
		for slot, source := range workspace.Sources {
			if err := ValidateWorkspaceSourceMode(source.Mode); err != nil {
				// Before modes were validated, an unknown mode on a git source
				// was materialized as a clone, which is what is on disk now.
				return fmt.Errorf("workspace %q source %q: %w (an existing workspace with this mode was cloned: set mode: clone or remove the line to keep it as it is)", name, slot, err)
			}
			if err := ValidateWorkspaceSourceVCS(source.VCS); err != nil {
				return fmt.Errorf("workspace %q source %q: %w", name, slot, err)
			}
		}
	}
	return nil
}

func (s *Stack) validateDependencyGraph() error {
	// No capacity hint: a summed len() hint trips CodeQL's
	// go/allocation-size-overflow, and the map grows fine without one.
	deps := make(map[string][]string)
	for name, service := range s.Services {
		if _, collision := s.Jobs[name]; collision {
			return fmt.Errorf("name %q is declared as both a service and a job", name)
		}
		deps[name] = append(append([]string{}, service.After...), service.DependsOn...)
	}
	for name, job := range s.Jobs {
		deps[name] = append([]string{}, job.DependsOn...)
	}
	for name, dependencies := range deps {
		for _, dependency := range dependencies {
			if _, ok := deps[dependency]; !ok {
				return fmt.Errorf("%s %q depends on unknown node %q", nodeKind(s, name), name, dependency)
			}
			if job, ok := s.Jobs[dependency]; ok && job.Capture != nil {
				return fmt.Errorf("%s %q cannot depend on manual capture job %q", nodeKind(s, name), name, dependency)
			}
		}
	}
	state := map[string]uint8{}
	var visit func(string) error
	visit = func(name string) error {
		if state[name] == 1 {
			return fmt.Errorf("dependency graph contains a cycle at %q", name)
		}
		if state[name] == 2 {
			return nil
		}
		state[name] = 1
		for _, dependency := range deps[name] {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[name] = 2
		return nil
	}
	for name := range deps {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

func nodeKind(s *Stack, name string) string {
	if _, ok := s.Jobs[name]; ok {
		return "job"
	}
	return "service"
}

func (s *Stack) validateServiceRuntime() error {
	for name, service := range s.Services {
		if err := validateReadyProbe(name, service.Ready); err != nil {
			return err
		}
		if service.StopGracePeriod != "" {
			duration, err := time.ParseDuration(service.StopGracePeriod)
			if err != nil || duration <= 0 {
				return fmt.Errorf("service %q: stop_grace_period must be a positive duration", name)
			}
		}
		switch service.Restart {
		case "", RestartNo, RestartOnFailure, RestartAlways:
		default:
			return fmt.Errorf("service %q: restart must be no, on_failure or always, not %q", name, service.Restart)
		}
	}
	return nil
}

func validateReadyProbe(serviceName string, probe *ReadyProbe) error {
	if probe == nil {
		return nil
	}

	kinds := 0
	if probe.HTTP != nil {
		kinds++
	}
	if probe.TCP != nil {
		kinds++
	}
	if probe.Cmd != nil {
		kinds++
	}
	if probe.File != "" {
		kinds++
	}
	if kinds != 1 {
		return fmt.Errorf("service %q: ready must set exactly one of http, tcp, cmd, or file", serviceName)
	}

	if probe.HTTP != nil && (probe.HTTP.Port < 1 || probe.HTTP.Port > 65535) {
		return fmt.Errorf("service %q: ready.http.port must be between 1 and 65535", serviceName)
	}
	if probe.TCP != nil && (probe.TCP.Port < 1 || probe.TCP.Port > 65535) {
		return fmt.Errorf("service %q: ready.tcp.port must be between 1 and 65535", serviceName)
	}
	if probe.Cmd != nil && len(probe.Cmd) == 0 {
		return fmt.Errorf("service %q: ready.cmd must contain at least one argument", serviceName)
	}
	if probe.File != "" && strings.TrimSpace(probe.File) == "" {
		return fmt.Errorf("service %q: ready.file must not be empty", serviceName)
	}

	normalized := probe.Normalized()
	durations := []struct {
		name  string
		value string
	}{
		{name: "interval", value: normalized.Interval},
		{name: "timeout", value: normalized.Timeout},
		{name: "start_period", value: normalized.StartPeriod},
	}
	for _, duration := range durations {
		if _, err := time.ParseDuration(duration.value); err != nil {
			return fmt.Errorf("service %q: ready.%s must be a valid duration: %w", serviceName, duration.name, err)
		}
	}
	if normalized.Retries == nil || *normalized.Retries < 1 {
		return fmt.Errorf("service %q: ready.retries must be at least 1", serviceName)
	}
	return nil
}

func validateRunnable(kind, name string, runtime Runtime, image string, build any, command []string) error {
	switch runtime {
	case RuntimeContainer:
		if image == "" && build == nil {
			return fmt.Errorf("%s %q with runtime container requires image or build", kind, name)
		}
	case RuntimeLocal:
		if image != "" {
			return fmt.Errorf("%s %q with runtime local must not set image", kind, name)
		}
		if len(command) == 0 {
			return fmt.Errorf("%s %q with runtime local requires command", kind, name)
		}
	case "":
		return fmt.Errorf("%s %q requires runtime", kind, name)
	default:
		return fmt.Errorf("%s %q has unsupported runtime %q", kind, name, runtime)
	}
	return nil
}

func (s *Stack) initMaps() {
	if s.Secrets == nil {
		s.Secrets = map[string]Secret{}
	}
	if s.Ports == nil {
		s.Ports = map[string]Port{}
	}
	if s.Volumes == nil {
		s.Volumes = map[string]Volume{}
	}
	if s.Persist == nil {
		s.Persist = map[string]PersistPath{}
	}
	if s.Sources == nil {
		s.Sources = map[string]Source{}
	}
	if s.WorkspaceDefaults == nil {
		s.WorkspaceDefaults = map[string]WorkspaceDefaults{}
	}
	if s.Workspaces == nil {
		s.Workspaces = map[string]Workspace{}
	}
	if s.Services == nil {
		s.Services = map[string]Service{}
	}
	if s.Jobs == nil {
		s.Jobs = map[string]Job{}
	}
	if s.PortLeases == nil {
		s.PortLeases = map[string][]PortLease{}
	}
}

func validCaptureName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\:")
}
