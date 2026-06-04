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
	client := git.New()
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil {
		if err := client.Fetch(ctx, repoDir); err != nil {
			return "", "", err
		}
		if branch != "" {
			if _, err := client.Run(ctx, repoDir, "checkout", branch); err != nil {
				return "", "", err
			}
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
			return "", "", err
		}
		if err := client.CloneRef(ctx, repoURL, repoDir, branch); err != nil {
			return "", "", err
		}
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
