package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ang-ee/angee-operator/internal/git"
)

// The default template registry: the repository that bare and kind-qualified
// template names (`dev`, `stacks/dev`) resolve from when no local template
// answers. Tests and enterprises override it with ANGEE_TEMPLATE_REGISTRY —
// a clone URL, an `owner/repo` GitHub shorthand, or a local repository path.
const defaultTemplateRegistry = "https://github.com/ang-ee/angee-templates.git"

const templateRegistryEnv = "ANGEE_TEMPLATE_REGISTRY"

func templateRegistryRepo() string {
	override := os.Getenv(templateRegistryEnv)
	if override == "" {
		return defaultTemplateRegistry
	}
	if strings.Contains(override, "://") || filepath.IsAbs(override) {
		return override
	}
	return "https://github.com/" + strings.TrimSuffix(override, ".git") + ".git"
}

func isRemoteTemplateRef(ref string) bool {
	u, err := url.Parse(ref)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http")
}

func (p *Platform) resolveRemoteTemplate(ctx context.Context, ref, kind string) (string, string, error) {
	repoURL, branch, subpath, err := parseGitHubTemplateRef(ref)
	if err != nil {
		return "", "", err
	}
	cacheRoot, err := templateCacheRoot(ref)
	if err != nil {
		return "", "", err
	}
	repoDir := filepath.Join(cacheRoot, "repo")
	if err := refreshTemplateRepo(ctx, repoURL, repoDir, branch); err != nil {
		return "", "", err
	}
	templatePath := filepath.Join(repoDir, filepath.FromSlash(subpath))
	if _, err := os.Stat(filepath.Join(templatePath, "copier.yml")); err != nil {
		if alt := alternateTemplatePath(repoDir, subpath, kind); alt != "" {
			templatePath = alt
		} else {
			return "", "", fmt.Errorf("template %q was not found in cloned repository", ref)
		}
	}
	return templatePath, ref, nil
}

// refreshTemplateRepo clones repoURL into repoDir, or refreshes an existing
// clone, leaving the worktree detached at ref. A branch ref tracks its remote
// (`origin/<ref>`) and the empty ref tracks the remote default branch, so a
// moving ref never serves a stale cache; a tag or SHA detaches at it verbatim.
func refreshTemplateRepo(ctx context.Context, repoURL, repoDir, ref string) error {
	client := git.New()
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil {
		if err := client.Fetch(ctx, repoDir); err != nil {
			return err
		}
		target := ref
		if target == "" {
			_, _ = client.Run(ctx, repoDir, "remote", "set-head", "origin", "--auto")
			target = "origin/HEAD"
		} else if _, err := client.Run(ctx, repoDir, "rev-parse", "--verify", "refs/remotes/origin/"+ref); err == nil {
			target = "origin/" + ref
		}
		_, err := client.Run(ctx, repoDir, "checkout", "--detach", target)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		return err
	}
	return client.CloneRef(ctx, repoURL, repoDir, ref)
}

// resolveRegistryTemplate resolves a name-shaped template ref from the
// template registry after every local candidate missed. Accepted shapes:
//
//	dev                     bare name → <kind>s/dev in the registry
//	stacks/dev              kind-qualified name in the registry
//	owner/repo//sub/path    an explicit template subpath in another GitHub repo
//	<any of the above>@ref  pinned to a branch, tag, or SHA
//
// The returned active ref is the kind-qualified NAME (never the repo URL), so
// a stack keeps resolving it locally first — its `templates` symlink into the
// materialized registry source — and re-reaches the registry only on a miss.
func (p *Platform) resolveRegistryTemplate(ctx context.Context, ref, kind string) (string, string, error) {
	base, pin := splitTemplateRefPin(ref)
	repoURL := templateRegistryRepo()
	family := kind + "s"
	activeRef := ref
	var candidates []string
	if owner, repo, sub, ok := splitOwnerRepoTemplateRef(base); ok {
		// Explicit cross-repository subpath: taken verbatim, no kind naming.
		repoURL = "https://github.com/" + owner + "/" + repo + ".git"
		candidates = []string{filepath.FromSlash(sub)}
	} else {
		kindRef := base
		if !strings.Contains(base, "/") {
			kindRef = family + "/" + base
		}
		if !strings.HasPrefix(kindRef, family+"/") {
			return "", "", fmt.Errorf("template %q does not match kind %q", ref, kind)
		}
		activeRef = kindRef
		candidates = []string{
			filepath.Join("templates", filepath.FromSlash(kindRef)),
			filepath.Join(".templates", filepath.FromSlash(kindRef)),
			filepath.FromSlash(kindRef),
		}
	}
	cacheRoot, err := templateCacheRoot(repoURL + "#" + pin)
	if err != nil {
		return "", "", err
	}
	repoDir := filepath.Join(cacheRoot, "repo")
	if err := refreshTemplateRepo(ctx, repoURL, repoDir, pin); err != nil {
		return "", "", err
	}
	for _, candidate := range candidates {
		templatePath := filepath.Join(repoDir, candidate)
		if _, err := os.Stat(filepath.Join(templatePath, "copier.yml")); err == nil {
			return templatePath, activeRef, nil
		}
	}
	return "", "", fmt.Errorf("template %q was not found in the template registry %s", ref, repoURL)
}

// splitTemplateRefPin splits a trailing `@<gitref>` pin off a name-shaped
// template ref. The pin must not contain a slash, so scoped names and paths
// pass through untouched.
func splitTemplateRefPin(ref string) (string, string) {
	at := strings.LastIndex(ref, "@")
	if at <= 0 || strings.Contains(ref[at+1:], "/") {
		return ref, ""
	}
	return ref[:at], ref[at+1:]
}

// splitOwnerRepoTemplateRef recognizes the explicit `owner/repo//subpath`
// cross-repository form. The double slash is required — a plain two-segment
// ref is always a kind-qualified name, never a repository.
func splitOwnerRepoTemplateRef(ref string) (owner, repo, subpath string, ok bool) {
	head, tail, found := strings.Cut(ref, "//")
	if !found || tail == "" {
		return "", "", "", false
	}
	parts := strings.Split(strings.Trim(head, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), strings.Trim(tail, "/"), true
}

func parseGitHubTemplateRef(ref string) (repoURL string, branch string, subpath string, err error) {
	u, err := url.Parse(ref)
	if err != nil {
		return "", "", "", err
	}
	if u.Host != "github.com" {
		return "", "", "", fmt.Errorf("remote template host %q is not supported", u.Host)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 3 {
		return "", "", "", fmt.Errorf("GitHub template URL must include owner, repo, and template path")
	}
	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")
	rest := parts[2:]
	if len(rest) >= 3 && rest[0] == "tree" {
		branch = rest[1]
		rest = rest[2:]
	}
	if queryRef := u.Query().Get("ref"); queryRef != "" {
		branch = queryRef
	}
	if len(rest) == 0 {
		return "", "", "", fmt.Errorf("GitHub template URL must include a template path")
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, repo), branch, strings.Join(rest, "/"), nil
}

func templateCacheRoot(ref string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	sum := sha256.Sum256([]byte(ref))
	return filepath.Join(base, "angee", "templates", hex.EncodeToString(sum[:12])), nil
}

func alternateTemplatePath(repoDir string, subpath string, kind string) string {
	candidates := []string{}
	if kind != "" {
		candidates = append(candidates,
			strings.Replace(subpath, "/.templates/"+kind+"/", "/.templates/"+kind+"s/", 1),
			strings.Replace(subpath, "/templates/"+kind+"/", "/templates/"+kind+"s/", 1),
		)
	}
	for _, candidate := range candidates {
		if candidate == subpath {
			continue
		}
		path := filepath.Join(repoDir, filepath.FromSlash(candidate))
		if _, err := os.Stat(filepath.Join(path, "copier.yml")); err == nil {
			return path
		}
	}
	return ""
}
