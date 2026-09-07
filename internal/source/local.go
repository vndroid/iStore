// Package source resolves a request path to a file on local disk.
//
// The only job here that matters is refusing to serve anything outside the
// configured root. Two ways out of a root exist and both are closed below:
// `..` segments in the request path, and symlinks inside the root pointing
// outside it. Cleaning the path handles the first; evaluating symlinks and
// re-checking handles the second.
package source

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrNotFound means no such file under the root.
	ErrNotFound = errors.New("source: not found")
	// ErrOutsideRoot means the resolved path escaped the root.
	ErrOutsideRoot = errors.New("source: path escapes the root directory")
	// ErrNotRegular means the path exists but is a directory, device, socket...
	ErrNotRegular = errors.New("source: not a regular file")
)

// Local serves files from a single directory tree.
type Local struct {
	root string // absolute, symlinks already evaluated
}

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

// File is an opened source file plus the identity used for cache keys.
type File struct {
	Path    string // absolute path on disk
	Size    int64
	ModTime int64 // unix nanoseconds
	f       *os.File
}

func (f *File) Read(p []byte) (int, error)                { return f.f.Read(p) }
func (f *File) Seek(off int64, whence int) (int64, error) { return f.f.Seek(off, whence) }
func (f *File) Close() error                              { return f.f.Close() }
func (f *File) WriteTo(w io.Writer) (int64, error)        { return io.Copy(w, f.f) }

// Open resolves urlPath (the request's URL path, still percent-encoded is fine —
// net/http has already decoded it into r.URL.Path) and opens the file.
func (l *Local) Open(urlPath string) (*File, error) {
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

	return &File{
		Path:    name,
		Size:    st.Size(),
		ModTime: st.ModTime().UnixNano(),
		f:       f,
	}, nil
}

// Stat resolves and stats without opening.
func (l *Local) Stat(urlPath string) (string, os.FileInfo, error) {
	name, err := l.resolve(urlPath)
	if err != nil {
		return "", nil, err
	}
	st, err := os.Stat(name)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, ErrNotFound
		}
		return "", nil, err
	}
	return name, st, nil
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
