package provision

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/fyltr/angee/internal/config"
	"github.com/fyltr/angee/internal/git"
)

// MaterializeSources ensures declared source trees exist under baseDir.
// Existing targets are preserved unless sync is true.
func MaterializeSources(ctx context.Context, baseDir string, sources map[string]config.SourceSpec, sync bool) ([]string, error) {
	var changed []string
	for _, name := range sortedKeys(sources) {
		path, ok, err := materializeSource(ctx, baseDir, name, sources[name], sync)
		if err != nil {
			return nil, err
		}
		if ok {
			changed = append(changed, path)
		}
	}
	return changed, nil
}

func materializeSource(ctx context.Context, baseDir, name string, source config.SourceSpec, sync bool) (string, bool, error) {
	target, err := sourceTarget(baseDir, source.Target)
	if err != nil {
		return "", false, fmt.Errorf("source %q: %w", name, err)
	}
	exists, err := pathExists(target)
	if err != nil {
		return "", false, fmt.Errorf("source %q: %w", name, err)
	}

	switch source.Kind {
	case "local":
		if source.Path == "" {
			if exists {
				return target, false, nil
			}
			return target, true, os.MkdirAll(target, 0755)
		}
		if exists && !sync {
			return target, false, nil
		}
		src, err := sourcePath(baseDir, source.Path, source.Tree)
		if err != nil {
			return "", false, fmt.Errorf("source %q: %w", name, err)
		}
		if err := copyTree(src, target, sync); err != nil {
			return "", false, fmt.Errorf("source %q: %w", name, err)
		}
		return target, true, nil
	case "git", "github":
		if exists && !sync {
			return target, false, nil
		}
		if exists && source.Tree != "" && source.Tree != "." {
			if err := os.RemoveAll(target); err != nil {
				return "", false, fmt.Errorf("source %q: %w", name, err)
			}
			if err := cloneSource(ctx, source, target); err != nil {
				return "", false, fmt.Errorf("source %q: %w", name, err)
			}
			return target, true, nil
		}
		if exists && !isGitRepo(target) {
			return "", false, fmt.Errorf("source %q: target %s exists and is not a git repository", name, target)
		}
		if exists {
			if err := git.New(target).SyncCtx(ctx, source.Ref); err != nil {
				return "", false, fmt.Errorf("source %q: %w", name, err)
			}
			return target, true, nil
		}
		if err := cloneSource(ctx, source, target); err != nil {
			return "", false, fmt.Errorf("source %q: %w", name, err)
		}
		return target, true, nil
	case "template", "volume":
		if exists {
			return target, false, nil
		}
		return target, true, os.MkdirAll(target, 0755)
	case "url":
		if exists && !sync {
			return target, false, nil
		}
		if err := materializeURL(ctx, source, target, sync); err != nil {
			return "", false, fmt.Errorf("source %q: %w", name, err)
		}
		return target, true, nil
	case "archive":
		if exists && !sync {
			return target, false, nil
		}
		if exists {
			if err := os.RemoveAll(target); err != nil {
				return "", false, fmt.Errorf("source %q: %w", name, err)
			}
		}
		if err := materializeArchive(ctx, baseDir, source, target); err != nil {
			return "", false, fmt.Errorf("source %q: %w", name, err)
		}
		return target, true, nil
	default:
		return "", false, fmt.Errorf("source %q: source kind %q is not implemented", name, source.Kind)
	}
}

func materializeURL(ctx context.Context, source config.SourceSpec, target string, sync bool) error {
	if source.URL == "" {
		return fmt.Errorf("url source requires url")
	}
	if sync {
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(target, 0755); err != nil {
		return err
	}
	filename := urlFilename(source.URL)
	if filename == "" {
		filename = "download"
	}
	return downloadFile(ctx, source.URL, filepath.Join(target, filename))
}

func materializeArchive(ctx context.Context, baseDir string, source config.SourceSpec, target string) error {
	archivePath := source.Path
	cleanup := func() {}
	if source.URL != "" {
		tmp, err := os.CreateTemp("", "angee-archive-*")
		if err != nil {
			return err
		}
		archivePath = tmp.Name()
		if err := tmp.Close(); err != nil {
			return err
		}
		cleanup = func() { _ = os.Remove(archivePath) }
		if err := downloadFile(ctx, source.URL, archivePath); err != nil {
			cleanup()
			return err
		}
	}
	defer cleanup()
	if archivePath == "" {
		return fmt.Errorf("archive source requires path or url")
	}
	archivePath = expandSourcePath(archivePath)
	if !filepath.IsAbs(archivePath) {
		archivePath = filepath.Join(baseDir, filepath.FromSlash(archivePath))
	}
	if err := os.MkdirAll(target, 0755); err != nil {
		return err
	}
	if strings.HasSuffix(archivePath, ".zip") {
		return extractZip(archivePath, target)
	}
	if strings.HasSuffix(archivePath, ".tar.gz") || strings.HasSuffix(archivePath, ".tgz") {
		return extractTarGz(archivePath, target)
	}
	if strings.HasSuffix(archivePath, ".tar") {
		return extractTar(archivePath, target)
	}
	return fmt.Errorf("unsupported archive format %s", archivePath)
}

func downloadFile(ctx context.Context, rawURL string, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download %s: HTTP %d", rawURL, resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func urlFilename(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return filepath.Base(parsed.Path)
}

func cloneSource(ctx context.Context, source config.SourceSpec, target string) error {
	url := sourceURL(source)
	if url == "" {
		return fmt.Errorf("git/github source requires repo or url")
	}
	ref := source.Ref
	if ref == "current" {
		ref = ""
	}
	if source.Tree == "" || source.Tree == "." {
		return git.CloneCtx(ctx, url, target, ref)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(target), ".angee-source-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := git.CloneCtx(ctx, url, tmp, ref); err != nil {
		return err
	}
	return copyTree(filepath.Join(tmp, filepath.FromSlash(source.Tree)), target, true)
}

func sourceURL(source config.SourceSpec) string {
	if source.URL != "" {
		return source.URL
	}
	if source.Kind == "github" && source.Repo != "" && !strings.Contains(source.Repo, "://") && !strings.HasSuffix(source.Repo, ".git") {
		return "https://github.com/" + source.Repo + ".git"
	}
	return source.Repo
}

func extractZip(path, target string) error {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	for _, file := range reader.File {
		dst, err := safeExtractPath(target, file.Name)
		if err != nil {
			return err
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(dst, file.Mode().Perm()); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		rc, err := file.Open()
		if err != nil {
			return err
		}
		if err := writeReader(dst, rc, file.Mode().Perm()); err != nil {
			_ = rc.Close()
			return err
		}
		if err := rc.Close(); err != nil {
			return err
		}
	}
	return nil
}

func extractTarGz(path, target string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	return extractTarReader(reader, target)
}

func extractTar(path, target string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return extractTarReader(file, target)
}

func extractTarReader(reader io.Reader, target string) error {
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		dst, err := safeExtractPath(target, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, safeArchivePerm(header.Mode, 0755)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				return err
			}
			if err := writeReader(dst, tarReader, safeArchivePerm(header.Mode, 0644)); err != nil {
				return err
			}
		}
	}
}

func safeArchivePerm(mode int64, fallback os.FileMode) os.FileMode {
	if mode < 0 || mode > 0777 {
		return fallback
	}
	return os.FileMode(mode).Perm()
}

func safeExtractPath(target, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("archive entry %q must be relative", name)
	}
	base, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	dst := filepath.Clean(filepath.Join(base, filepath.FromSlash(name)))
	rel, err := filepath.Rel(base, dst)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes target", name)
	}
	return dst, nil
}

func writeReader(dst string, src io.Reader, perm os.FileMode) error {
	if perm == 0 {
		perm = 0644
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm)
}

func sourceTarget(baseDir, target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("target is required")
	}
	if filepath.IsAbs(target) {
		return "", fmt.Errorf("target %q must be relative", target)
	}
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	joined := filepath.Clean(filepath.Join(base, filepath.FromSlash(target)))
	rel, err := filepath.Rel(base, joined)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("target %q escapes %s", target, baseDir)
	}
	return joined, nil
}

func sourcePath(baseDir, path, tree string) (string, error) {
	path = expandSourcePath(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, filepath.FromSlash(path))
	}
	if tree != "" && tree != "." {
		path = filepath.Join(path, filepath.FromSlash(tree))
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("source path %s is not a directory", path)
	}
	return path, nil
}

func expandSourcePath(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func copyTree(src, dst string, overwrite bool) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0755)
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return copySymlink(path, target, overwrite)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(path, target, info.Mode().Perm(), overwrite)
	})
}

func copyFile(src, dst string, perm os.FileMode, overwrite bool) error {
	if _, err := os.Stat(dst); err == nil && !overwrite {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm)
}

func copySymlink(src, dst string, overwrite bool) error {
	if _, err := os.Lstat(dst); err == nil {
		if !overwrite {
			return nil
		}
		if err := os.Remove(dst); err != nil {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	link, err := os.Readlink(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	return os.Symlink(link, dst)
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func isGitRepo(path string) bool {
	info, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && info.IsDir()
}
