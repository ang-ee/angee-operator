package manifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"gopkg.in/yaml.v3"
)

const (
	KindStack      = "stack"
	VersionCurrent = 1
)

var routeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var routeHostPattern = regexp.MustCompile(`^[a-zA-Z0-9.-]+$`)

type Runtime string

const (
	RuntimeContainer Runtime = "container"
	RuntimeLocal     Runtime = "local"
)

type Stack struct {
	Version        int                    `yaml:"version" json:"version" validate:"oneof=1" jsonschema:"required,enum=1"`
	Kind           string                 `yaml:"kind" json:"kind" validate:"required,oneof=stack" jsonschema:"required,enum=stack"`
	Name           string                 `yaml:"name" json:"name"`
	Template       *Template              `yaml:"template,omitempty" json:"template,omitempty"`
	Operator       Operator               `yaml:"operator,omitempty" json:"operator,omitempty"`
	SecretsBackend SecretsBackend         `yaml:"secrets_backend,omitempty" json:"secrets_backend,omitempty"`
	Ingress        Ingress                `yaml:"ingress,omitempty" json:"ingress,omitempty"`
	Secrets        map[string]Secret      `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	Ports          map[string]Port        `yaml:"ports,omitempty" json:"ports,omitempty"`
	Volumes        map[string]Volume      `yaml:"volumes,omitempty" json:"volumes,omitempty"`
	Persist        map[string]PersistPath `yaml:"persist,omitempty" json:"persist,omitempty"`
	Sources        map[string]Source      `yaml:"sources,omitempty" json:"sources,omitempty"`
	Workspaces     map[string]Workspace   `yaml:"workspaces,omitempty" json:"workspaces,omitempty"`
	Services       map[string]Service     `yaml:"services,omitempty" json:"services,omitempty"`
	Jobs           map[string]Job         `yaml:"jobs,omitempty" json:"jobs,omitempty"`
	PortLeases     map[string][]PortLease `yaml:"port_leases,omitempty" json:"port_leases,omitempty"`
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

type Ingress struct {
	Type    string `yaml:"type,omitempty" json:"type,omitempty" validate:"omitempty,oneof=none caddy" jsonschema:"enum=none,enum=caddy"`
	Domain  string `yaml:"domain,omitempty" json:"domain,omitempty"`
	Image   string `yaml:"image,omitempty" json:"image,omitempty"`
	Network string `yaml:"network,omitempty" json:"network,omitempty"`
	Verify  string `yaml:"verify,omitempty" json:"verify,omitempty"`
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
	Kind       string     `yaml:"kind" json:"kind" validate:"required,oneof=git local" jsonschema:"required,enum=git,enum=local"`
	Repo       string     `yaml:"repo,omitempty" json:"repo,omitempty"`
	URL        string     `yaml:"url,omitempty" json:"url,omitempty"`
	Path       string     `yaml:"path,omitempty" json:"path,omitempty"`
	DefaultRef string     `yaml:"default_ref,omitempty" json:"default_ref,omitempty"`
	CachePath  string     `yaml:"cache_path,omitempty" json:"cache_path,omitempty"`
	Auth       SourceAuth `yaml:"auth,omitempty" json:"auth,omitempty"`
	Git        SourceGit  `yaml:"git,omitempty" json:"git,omitempty"`
	Checksum   string     `yaml:"checksum,omitempty" json:"checksum,omitempty"`
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

type Workspace struct {
	Template     string                     `yaml:"template" json:"template" validate:"required" jsonschema:"required"`
	Inputs       map[string]string          `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Sources      map[string]WorkspaceSource `yaml:"sources,omitempty" json:"sources,omitempty"`
	Resolved     WorkspaceResolved          `yaml:"resolved,omitempty" json:"resolved,omitempty"`
	TTL          string                     `yaml:"ttl,omitempty" json:"ttl,omitempty"`
	TTLExpiresAt *time.Time                 `yaml:"ttl_expires_at,omitempty" json:"ttl_expires_at,omitempty"`
}

type WorkspaceSource struct {
	Source  string `yaml:"source" json:"source" validate:"required" jsonschema:"required"`
	Mode    string `yaml:"mode,omitempty" json:"mode,omitempty"`
	Branch  string `yaml:"branch,omitempty" json:"branch,omitempty"`
	Ref     string `yaml:"ref,omitempty" json:"ref,omitempty"`
	Subpath string `yaml:"subpath,omitempty" json:"subpath,omitempty"`
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
}

type Route struct {
	Port int    `yaml:"port" json:"port" validate:"required,gte=1,lte=65535" jsonschema:"required,minimum=1,maximum=65535"`
	Host string `yaml:"host,omitempty" json:"host,omitempty"`
	Auth string `yaml:"auth,omitempty" json:"auth,omitempty" validate:"omitempty,oneof=forward none" jsonschema:"enum=forward,enum=none"`
}

type Job struct {
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
	for name, service := range s.Services {
		if service.Route != nil && service.Runtime == RuntimeLocal {
			return fmt.Errorf("service %q: route requires runtime: container", name)
		}
		if err := validateRunnable("service", name, service.Runtime, service.Image, service.Build, service.Command); err != nil {
			return err
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
			if service.Route.Host != "" && !routeHostPattern.MatchString(service.Route.Host) {
				return fmt.Errorf("service %q: route.host %q contains invalid characters", name, service.Route.Host)
			}
		}
	}
	for name, job := range s.Jobs {
		if err := validateRunnable("job", name, job.Runtime, job.Image, job.Build, job.Command); err != nil {
			return err
		}
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
