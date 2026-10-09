package mount

import (
	"fmt"
	"path/filepath"
	"strings"
)

type Resolver struct {
	Workspaces map[string]string
	Sources    map[string]string
	Volumes    map[string]string
}

type Mount struct {
	Scheme   string
	Name     string
	Subpath  string
	HostPath string
	Target   string
	ReadOnly bool
}

func Parse(raw string) (Mount, error) {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return Mount{}, fmt.Errorf("mount %q is missing scheme", raw)
	}
	if scheme == "bind" {
		host, target, readOnly, err := splitTarget(rest)
		if err != nil {
			return Mount{}, err
		}
		return Mount{Scheme: scheme, HostPath: host, Target: target, ReadOnly: readOnly}, nil
	}
	left, target, readOnly, err := splitTarget(rest)
	if err != nil {
		return Mount{}, err
	}
	name, subpath, _ := strings.Cut(left, "/")
	if name == "" {
		return Mount{}, fmt.Errorf("mount %q is missing resource name", raw)
	}
	return Mount{Scheme: scheme, Name: name, Subpath: subpath, Target: target, ReadOnly: readOnly}, nil
}

// BindSource is a bind mount's host path as compose takes it: a relative path
// is made explicit ("./data"), since compose reads a bare name as a volume.
func (m Mount) BindSource() string {
	host := filepath.Clean(m.HostPath)
	if !filepath.IsAbs(host) && !strings.HasPrefix(host, ".") {
		host = "." + string(filepath.Separator) + host
	}
	return host
}

func ResolveContainer(raw string, resolver Resolver) (string, error) {
	m, err := Parse(raw)
	if err != nil {
		return "", err
	}
	suffix := ""
	if m.ReadOnly {
		suffix = ":ro"
	}
	switch m.Scheme {
	case "workspace":
		host, err := resourcePath(resolver.Workspaces, m.Name, m.Subpath, "workspace")
		if err != nil {
			return "", err
		}
		return host + ":" + m.Target + suffix, nil
	case "source":
		host, err := resourcePath(resolver.Sources, m.Name, m.Subpath, "source")
		if err != nil {
			return "", err
		}
		return host + ":" + m.Target + suffix, nil
	case "volume":
		if m.Subpath != "" {
			return "", fmt.Errorf("volume mounts do not support subpaths: %q", raw)
		}
		return m.Name + ":" + m.Target + suffix, nil
	case "bind":
		return m.BindSource() + ":" + m.Target + suffix, nil
	default:
		return "", fmt.Errorf("unsupported mount scheme %q", m.Scheme)
	}
}

func ResolveLocalEnv(raw string, resolver Resolver) (string, string, error) {
	m, err := Parse(raw)
	if err != nil {
		return "", "", err
	}
	switch m.Scheme {
	case "workspace":
		host, err := resourcePath(resolver.Workspaces, m.Name, m.Subpath, "workspace")
		return envName("WORKSPACE", m.Name, m.Subpath), host, err
	case "source":
		host, err := resourcePath(resolver.Sources, m.Name, m.Subpath, "source")
		return envName("SOURCE", m.Name, m.Subpath), host, err
	case "volume":
		host, err := resourcePath(resolver.Volumes, m.Name, m.Subpath, "volume")
		return envName("VOLUME", m.Name, m.Subpath), host, err
	case "bind":
		return envName("BIND", strings.Trim(m.HostPath, "/"), ""), filepath.Clean(m.HostPath), nil
	default:
		return "", "", fmt.Errorf("unsupported mount scheme %q", m.Scheme)
	}
}

func ResolveWorkdir(raw string, resolver Resolver) (string, error) {
	if raw == "" || !strings.Contains(raw, "://") {
		return raw, nil
	}
	scheme, rest, _ := strings.Cut(raw, "://")
	if scheme == "bind" {
		return filepath.Clean(rest), nil
	}
	name, subpath, _ := strings.Cut(rest, "/")
	switch scheme {
	case "workspace":
		return resourcePath(resolver.Workspaces, name, subpath, "workspace")
	case "source":
		return resourcePath(resolver.Sources, name, subpath, "source")
	case "volume":
		return resourcePath(resolver.Volumes, name, subpath, "volume")
	default:
		return "", fmt.Errorf("unsupported workdir scheme %q", scheme)
	}
}

func splitTarget(rest string) (string, string, bool, error) {
	left, right, ok := strings.Cut(rest, ":")
	if !ok || left == "" || right == "" {
		return "", "", false, fmt.Errorf("mount %q must have source:/target", rest)
	}
	readOnly := false
	if strings.HasSuffix(right, ":ro") {
		readOnly = true
		right = strings.TrimSuffix(right, ":ro")
	}
	if !strings.HasPrefix(right, "/") {
		return "", "", false, fmt.Errorf("mount target %q must be absolute", right)
	}
	return left, right, readOnly, nil
}

func resourcePath(resources map[string]string, name, subpath, kind string) (string, error) {
	base, ok := resources[name]
	if !ok {
		return "", fmt.Errorf("%s %q is not declared", kind, name)
	}
	if subpath == "" {
		return filepath.Clean(base), nil
	}
	return filepath.Clean(filepath.Join(base, subpath)), nil
}

func envName(prefix, name, subpath string) string {
	parts := []string{prefix, name}
	if subpath != "" {
		parts = append(parts, strings.Split(subpath, "/")...)
	}
	parts = append(parts, "PATH")
	for i, part := range parts {
		parts[i] = sanitize(part)
	}
	return strings.Join(parts, "_")
}

func sanitize(value string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(value) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return strings.Trim(b.String(), "_")
}
