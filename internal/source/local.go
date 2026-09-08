package source

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Local serves objects from a single directory tree.
//
// The only job here that matters is refusing to serve anything outside the
// configured root. Two ways out of a root exist and both are closed below:
// `..` segments in the request path, and symlinks inside the root pointing
// outside it. Cleaning the path handles the first; evaluating symlinks and
// re-checking handles the second.
type Local struct {
	root string // absolute, symlinks already evaluated
}

var _ Source = (*Local)(nil)

// NewLocal opens root, which must be an existing directory.
func NewLocal(root string) (*Local, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Evaluate the root's own symlinks once, so later comparisons are made
	// between two fully-resolved paths.
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("source: root %q: %w", root, err)
	}
	st, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("source: root %q is not a directory", root)
	}
	return &Local{root: real}, nil
}

// Root is the resolved root directory.
func (l *Local) Root() string { return l.root }

// Describe implements Source.
func (l *Local) Describe() string { return l.root }

// Stat resolves and stats without opening.
func (l *Local) Stat(_ context.Context, urlPath string) (*Info, error) {
	name, err := l.resolve(urlPath)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, ErrNotRegular
	}
	return &Info{
		Key:     name,
		Size:    st.Size(),
		Version: strconv.FormatInt(st.ModTime().UnixNano(), 10),
	}, nil
}

// OpenHeader is Open. A local read is already a seek and a 64 KiB copy, so
// there is nothing for a prefix to save — the distinction only pays against a
// network origin.
func (l *Local) OpenHeader(ctx context.Context, urlPath string, _ int) (*Object, error) {
	return l.Open(ctx, urlPath)
}

// Open resolves urlPath (the request's URL path, already decoded by net/http)
// and opens the file.
func (l *Local) Open(_ context.Context, urlPath string) (*Object, error) {
	name, err := l.resolve(urlPath)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, ErrNotRegular
	}

	return newObject(Info{
		Key:     name,
		Size:    st.Size(),
		Version: strconv.FormatInt(st.ModTime().UnixNano(), 10),
	}, f, f), nil
}

// resolve maps a URL path to an absolute path guaranteed to sit under the root.
func (l *Local) resolve(urlPath string) (string, error) {
	// A path that fails to unescape is a client error, not a 500.
	if u, err := url.PathUnescape(urlPath); err == nil {
		urlPath = u
	}

	// A NUL byte truncates the name in the syscall layer of some systems.
	if strings.ContainsRune(urlPath, 0) {
		return "", ErrOutsideRoot
	}

	// filepath.Clean on a path forced to be rooted collapses `..` without
	// letting it climb past the top: "/a/../../b" becomes "/b".
	clean := filepath.Clean("/" + strings.TrimPrefix(urlPath, "/"))
	name := filepath.Join(l.root, clean)

	// Join+Clean already prevents `..` escapes. The remaining hole is a symlink
	// inside the root pointing out of it, so resolve and re-check. A path that
	// does not exist yet cannot be a symlink escape, so a missing file is
	// reported as such rather than as an escape.
	real, err := filepath.EvalSymlinks(name)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	if real != l.root && !strings.HasPrefix(real, l.root+string(filepath.Separator)) {
		return "", ErrOutsideRoot
	}

	return real, nil
}
