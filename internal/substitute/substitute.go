package substitute

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosimple/slug"
)

var expressionRE = regexp.MustCompile(`\$\{([^{}]+)\}`)

type Context struct {
	Secrets       map[string]string
	SecretEnvVars map[string]string
	Services      map[string]Service
	Ports         map[string]int
	Alloc         map[string]int
	Workspaces    map[string]string
	WorkspacePath string
	Sources       map[string]string
	Persist       map[string]string
	Operator      Operator
	Inputs        map[string]string
	Name          string
}

type Service struct {
	Host string
	Port int
	URL  string
}

type Operator struct {
	URL    string
	Domain string
}

func Resolve(input string, ctx Context) (string, error) {
	var firstErr error
	resolved := expressionRE.ReplaceAllStringFunc(input, func(match string) string {
		if firstErr != nil {
			return match
		}
		expr := strings.TrimSpace(match[2 : len(match)-1])
		value, err := eval(expr, ctx)
		if err != nil {
			firstErr = err
			return match
		}
		return value
	})
	if firstErr != nil {
		return "", firstErr
	}
	return resolved, nil
}

func ResolveMap(input map[string]string, ctx Context) (map[string]string, error) {
	if len(input) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		resolved, err := Resolve(value, ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		out[key] = resolved
	}
	return out, nil
}

func ResolveSlice(input []string, ctx Context) ([]string, error) {
	if len(input) == 0 {
		return nil, nil
	}
	out := make([]string, len(input))
	for i, value := range input {
		resolved, err := Resolve(value, ctx)
		if err != nil {
			return nil, fmt.Errorf("index %d: %w", i, err)
		}
		out[i] = resolved
	}
	return out, nil
}

// SecretRefs returns the distinct secret names referenced as `${secret.NAME}`
// across inputs, in first-seen order. It parses the same marker grammar as
// Resolve (pipe filters after the name are ignored), so callers discover
// exactly the secrets resolution will require.
func SecretRefs(inputs ...string) []string {
	seen := map[string]struct{}{}
	var names []string
	for _, input := range inputs {
		for _, match := range expressionRE.FindAllStringSubmatch(input, -1) {
			parts := splitPipes(strings.TrimSpace(match[1]))
			if len(parts) == 0 {
				continue
			}
			ns, rest, ok := strings.Cut(strings.TrimSpace(parts[0]), ".")
			if !ok || ns != "secret" {
				continue
			}
			name := strings.TrimSpace(rest)
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	return names
}

func eval(expr string, ctx Context) (string, error) {
	parts := splitPipes(expr)
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return "", errors.New("empty substitution")
	}
	value, err := resolvePath(strings.TrimSpace(parts[0]), ctx)
	if err != nil {
		return "", err
	}
	for _, filter := range parts[1:] {
		value, err = applyFilter(value, strings.TrimSpace(filter))
		if err != nil {
			return "", err
		}
	}
	return value, nil
}

func resolvePath(path string, ctx Context) (string, error) {
	if path == "name" {
		return ctx.Name, nil
	}
	ns, rest, ok := strings.Cut(path, ".")
	if !ok {
		return "", fmt.Errorf("unknown substitution %q", path)
	}
	switch ns {
	case "secret":
		if env, ok := ctx.SecretEnvVars[rest]; ok {
			return "${" + env + "}", nil
		}
		value, ok := ctx.Secrets[rest]
		if !ok {
			return "", fmt.Errorf("secret %q is not resolved", rest)
		}
		return value, nil
	case "service":
		name, field, _ := strings.Cut(rest, ".")
		service, ok := ctx.Services[name]
		if !ok {
			return "", fmt.Errorf("service %q is not known", name)
		}
		if field == "" {
			if service.URL != "" {
				return service.URL, nil
			}
			if service.Port != 0 {
				return fmt.Sprintf("%s:%d", service.Host, service.Port), nil
			}
			return service.Host, nil
		}
		switch field {
		case "host":
			return service.Host, nil
		case "port":
			return strconv.Itoa(service.Port), nil
		case "url":
			return service.URL, nil
		default:
			return "", fmt.Errorf("unknown service field %q", field)
		}
	case "ports":
		value, ok := ctx.Ports[rest]
		if !ok {
			return "", fmt.Errorf("port %q is not declared", rest)
		}
		return strconv.Itoa(value), nil
	case "alloc":
		value, ok := ctx.Alloc[rest]
		if !ok {
			return "", fmt.Errorf("allocation %q is not declared", rest)
		}
		return strconv.Itoa(value), nil
	case "workspace":
		if rest == "path" && ctx.WorkspacePath != "" {
			return ctx.WorkspacePath, nil
		}
		name, field, _ := strings.Cut(rest, ".")
		value, ok := ctx.Workspaces[name]
		if !ok {
			return "", fmt.Errorf("workspace %q is not known", name)
		}
		if field == "" || field == "path" {
			return value, nil
		}
		return "", fmt.Errorf("unknown workspace field %q", field)
	case "source":
		name, field, _ := strings.Cut(rest, ".")
		value, ok := ctx.Sources[name]
		if !ok {
			return "", fmt.Errorf("source %q is not known", name)
		}
		if field == "" || field == "path" {
			return value, nil
		}
		return "", fmt.Errorf("unknown source field %q", field)
	case "persist":
		value, ok := ctx.Persist[rest]
		if !ok {
			return "", fmt.Errorf("persist path %q is not known", rest)
		}
		return value, nil
	case "operator":
		switch rest {
		case "url":
			return ctx.Operator.URL, nil
		case "domain":
			return ctx.Operator.Domain, nil
		default:
			return "", fmt.Errorf("unknown operator field %q", rest)
		}
	case "inputs":
		value, ok := ctx.Inputs[rest]
		if !ok {
			return "", fmt.Errorf("input %q is not set", rest)
		}
		return value, nil
	default:
		return "", fmt.Errorf("unknown substitution namespace %q", ns)
	}
}

func applyFilter(value, filter string) (string, error) {
	name, args := parseCall(filter)
	switch name {
	case "slug":
		return slug.Make(value), nil
	case "lower":
		return strings.ToLower(value), nil
	case "upper":
		return strings.ToUpper(value), nil
	case "local_part":
		local, _, _ := strings.Cut(value, "@")
		return local, nil
	case "truncate":
		if len(args) != 1 {
			return "", errors.New("truncate requires one argument")
		}
		limit, err := strconv.Atoi(args[0])
		if err != nil || limit < 0 {
			return "", fmt.Errorf("invalid truncate length %q", args[0])
		}
		return truncate(value, limit), nil
	case "default":
		if len(args) != 1 {
			return "", errors.New("default requires one argument")
		}
		if value == "" {
			return args[0], nil
		}
		return value, nil
	case "required":
		if len(args) > 1 {
			return "", errors.New("required accepts zero or one argument")
		}
		if value == "" {
			if len(args) == 1 && args[0] != "" {
				return "", errors.New(args[0])
			}
			return "", errors.New("required value is empty")
		}
		return value, nil
	case "b64encode":
		return base64.StdEncoding.EncodeToString([]byte(value)), nil
	case "replace":
		if len(args) != 2 {
			return "", errors.New("replace requires two arguments")
		}
		return strings.ReplaceAll(value, args[0], args[1]), nil
	default:
		return "", fmt.Errorf("unknown filter %q", name)
	}
}

func parseCall(filter string) (string, []string) {
	open := strings.IndexByte(filter, '(')
	if open == -1 || !strings.HasSuffix(filter, ")") {
		return filter, nil
	}
	name := strings.TrimSpace(filter[:open])
	body := strings.TrimSpace(filter[open+1 : len(filter)-1])
	if body == "" {
		return name, nil
	}
	return name, splitArgs(body)
}

func splitPipes(expr string) []string {
	var out []string
	var b strings.Builder
	quote := rune(0)
	depth := 0
	for _, r := range expr {
		switch {
		case quote != 0:
			b.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
			b.WriteRune(r)
		case r == '(':
			depth++
			b.WriteRune(r)
		case r == ')':
			if depth > 0 {
				depth--
			}
			b.WriteRune(r)
		case r == '|' && depth == 0:
			out = append(out, strings.TrimSpace(b.String()))
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	out = append(out, strings.TrimSpace(b.String()))
	return out
}

func splitArgs(body string) []string {
	var out []string
	var b strings.Builder
	quote := rune(0)
	for _, r := range body {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			b.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case r == ',':
			out = append(out, strings.TrimSpace(b.String()))
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	out = append(out, strings.TrimSpace(b.String()))
	return out
}

func truncate(value string, limit int) string {
	if limit == 0 {
		return ""
	}
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	var b strings.Builder
	count := 0
	for _, r := range value {
		if count >= limit {
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

func SecretEnvName(name string) string {
	var b strings.Builder
	b.WriteString("ANGEE_SECRET_")
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return strings.TrimRight(b.String(), "_")
}
