package source

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// newRoot builds a temporary tree:
//
//	root/ok.txt
//	root/sub/nested.txt
//	root/escape       -> ../outside/secret.txt   (symlink out of the root)
//	outside/secret.txt
func newRoot(t *testing.T) (root string, secret string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")

	for _, d := range []string{root, filepath.Join(root, "sub"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "ok.txt"), "ok")
	write(filepath.Join(root, "sub", "nested.txt"), "nested")
	secret = filepath.Join(outside, "secret.txt")
	write(secret, "secret")

	if runtime.GOOS != "windows" {
		if err := os.Symlink(secret, filepath.Join(root, "escape")); err != nil {
			t.Fatal(err)
		}
	}
	return root, secret
}

func TestOpenAllowsPathsInsideRoot(t *testing.T) {
	root, _ := newRoot(t)
	l, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"/ok.txt", "ok.txt", "/sub/nested.txt", "/sub/../ok.txt"} {
		f, err := l.Open(p)
		if err != nil {
			t.Errorf("Open(%q): %v", p, err)
			continue
		}
		f.Close()
	}
}

func TestOpenRejectsEscapes(t *testing.T) {
	root, _ := newRoot(t)
	l, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}

	cases := []string{
		"/../outside/secret.txt",
		"/sub/../../outside/secret.txt",
		"/../../etc/passwd",
		"////../outside/secret.txt",
		"/sub/./../../outside/secret.txt",
	}
	for _, p := range cases {
		f, err := l.Open(p)
		if err == nil {
			f.Close()
			t.Errorf("Open(%q): expected refusal, got success", p)
			continue
		}
		// A cleaned `..` path lands outside the root and simply does not exist
		// there, so ErrNotFound is the honest answer; ErrOutsideRoot is returned
		// when the path resolves onto something real out of bounds.
		if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("Open(%q): unexpected error %v", p, err)
		}
	}
}

func TestOpenRejectsSymlinkOutOfRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root, _ := newRoot(t)
	l, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}

	f, err := l.Open("/escape")
	if err == nil {
		f.Close()
		t.Fatal("Open(/escape): a symlink out of the root was served")
	}
	if !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("Open(/escape): got %v, want ErrOutsideRoot", err)
	}
}

func TestOpenRejectsDirectory(t *testing.T) {
	root, _ := newRoot(t)
	l, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	f, err := l.Open("/sub")
	if err == nil {
		f.Close()
		t.Fatal("Open(/sub): a directory was opened as a file")
	}
	if !errors.Is(err, ErrNotRegular) {
		t.Errorf("got %v, want ErrNotRegular", err)
	}
}

func TestOpenMissing(t *testing.T) {
	root, _ := newRoot(t)
	l, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Open("/nope.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestNewLocalRejectsFile(t *testing.T) {
	root, _ := newRoot(t)
	if _, err := NewLocal(filepath.Join(root, "ok.txt")); err == nil {
		t.Fatal("NewLocal accepted a regular file as the root")
	}
}
