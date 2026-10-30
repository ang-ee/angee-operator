package copierx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Aside is the directory a GuardedPath held, renamed under a private name in
// the same parent (SetAside), so that something new can be made at the path
// while the old directory, at the same depth, keeps working: a workspace
// slot migrated from one kind of checkout to another keeps its relative
// links to its repository, and its files are carried across one entry at a
// time (MoveFromAside). It keeps the identity of what it moved, and every
// method checks it first, so an entry swapped in under the private name is
// never acted on.
type Aside struct {
	parent   *os.Root
	name     string
	path     string
	identity fs.FileInfo
}

// SetAside renames the guarded directory, which path names, to a fresh name
// that starts with prefix in the same parent, and returns it. The guard then
// holds no entry, as after RemoveAll, so the caller can make a new one at the
// path through it. It refuses an entry that is not a real directory, and a
// path that no longer names the guarded entry.
func (p *GuardedPath) SetAside(prefix, path string) (*Aside, error) {
	if err := p.VerifyPathIdentity(path); err != nil {
		return nil, err
	}
	if p.entryRoot == nil {
		return nil, fmt.Errorf("guarded destination %q is not a real directory", p.leaf)
	}
	identity := p.entry
	name, moved, err := p.moveExpectedAside(prefix)
	if err != nil {
		return nil, err
	}
	if !moved {
		return nil, fmt.Errorf("guarded destination %q has nothing to set aside", p.leaf)
	}
	return &Aside{parent: p.parent, name: name, path: filepath.Join(filepath.Dir(path), name), identity: identity}, nil
}

// Path is where the directory set aside is, for a tool that must be handed a
// path, such as `git worktree repair`. Verify first.
func (a *Aside) Path() string { return a.path }

// Verify confirms that the private name, looked up in the parent and by its
// public path, still leads to the directory that was set aside.
func (a *Aside) Verify() error {
	current, err := a.parent.Lstat(a.name)
	if err != nil {
		return err
	}
	if !sameGuardedEntryIdentity(a.identity, current) {
		return fmt.Errorf("directory set aside as %q changed identity", a.name)
	}
	public, err := os.Lstat(a.path)
	if err != nil {
		return err
	}
	if !sameGuardedEntryIdentity(a.identity, public) {
		return fmt.Errorf("path %q no longer identifies the directory set aside", a.path)
	}
	return nil
}

// RestoreAside renames the directory set aside back to the guarded path,
// which must hold nothing, and the guard holds it again.
func (p *GuardedPath) RestoreAside(a *Aside) error {
	if err := p.sameParent(a); err != nil {
		return err
	}
	if err := a.Verify(); err != nil {
		return err
	}
	return p.restoreAside(a.name)
}

// MoveFromAside moves the entry rel of the directory set aside to the same
// place in the guarded directory: a rename within the parent, so nothing is
// copied. Every directory above rel must be a real directory on both sides,
// and nothing may be at rel in the guarded directory: an entry is never
// overwritten.
func (p *GuardedPath) MoveFromAside(a *Aside, rel string) error {
	return p.moveAside(a, rel, rel, true)
}

// MoveToAside moves the entry rel of the guarded directory back into the
// directory set aside, as MoveFromAside moves one out, undoing it.
func (p *GuardedPath) MoveToAside(a *Aside, rel string) error {
	return p.moveAside(a, rel, rel, false)
}

// MoveToAsideAs moves the entry rel of the guarded directory into the
// directory set aside as as, as MoveToAside moves it to rel: for an entry
// the directory set aside holds again, kept beside it under another name.
func (p *GuardedPath) MoveToAsideAs(a *Aside, rel, as string) error {
	return p.moveAside(a, rel, as, false)
}

// moveAside moves the entry rel of one side to target on the other: out of
// the directory set aside into the guarded one (out), else back.
func (p *GuardedPath) moveAside(a *Aside, rel, target string, out bool) error {
	for _, path := range []string{rel, target} {
		if err := validateRelativePath(path); err != nil {
			return err
		}
	}
	if err := p.sameParent(a); err != nil {
		return err
	}
	if err := a.Verify(); err != nil {
		return err
	}
	if err := p.verifyEntry(); err != nil {
		return err
	}
	if !p.exists || p.entryRoot == nil {
		return fmt.Errorf("guarded destination %q is not a real directory", p.leaf)
	}
	rel, target = filepath.Clean(filepath.FromSlash(rel)), filepath.Clean(filepath.FromSlash(target))
	from, to := a.name, p.leaf
	if !out {
		from, to = p.leaf, a.name
	}
	for _, side := range []struct{ base, rel string }{{from, rel}, {to, target}} {
		parts := splitPathParts(side.rel)
		for i := 1; i < len(parts); i++ {
			dir := filepath.Join(append([]string{side.base}, parts[:i]...)...)
			info, err := p.parent.Lstat(dir)
			if err != nil {
				return fmt.Errorf("move %s: %w", rel, err)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("move %s: %s is not a real directory", rel, dir)
			}
		}
	}
	source, dest := filepath.Join(from, rel), filepath.Join(to, target)
	if _, err := p.parent.Lstat(source); err != nil {
		return fmt.Errorf("move %s: %w", rel, err)
	}
	if _, err := p.parent.Lstat(dest); err == nil {
		return fmt.Errorf("move %s: %w", rel, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("move %s: %w", rel, err)
	}
	return p.parent.Rename(source, dest)
}

// RemoveAll removes the directory set aside, once its name still leads to it.
func (a *Aside) RemoveAll() error {
	if err := a.Verify(); err != nil {
		return err
	}
	return a.parent.RemoveAll(a.name)
}

// sameParent refuses an Aside another guard set aside.
func (p *GuardedPath) sameParent(a *Aside) error {
	if a == nil || p.parent == nil || a.parent != p.parent {
		return errors.New("the directory was not set aside by this guarded destination")
	}
	return nil
}
