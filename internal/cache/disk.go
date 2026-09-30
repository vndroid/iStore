// Package cache stores transcoded images on disk.
//
// The measured cost of one AVIF encode on 2 vCPU is ~150 ms for a 2400x1350
// frame. A page of twelve thumbnails is therefore ~1.8 CPU-seconds if nothing is
// cached — which is why this package is not optional, and why the HTTP layer
// wires it in before it wires in anything else.
//
// The key covers the source's identity and the exact process chain, so editing a
// source file or changing the chain misses naturally without any invalidation
// step. What "identity" means depends on where the source came from — path, size
// and mtime on local disk; the origin's validator, or a time bucket, upstream —
// see Key.
//
// Writes go to a temporary file in the same directory and are renamed into
// place, so a reader never sees a half-written entry and two concurrent writers
// of the same key cannot corrupt each other.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// ErrMiss reports that the key is not cached.
var ErrMiss = errors.New("cache: miss")

// Disk is a content-addressed cache rooted at a directory.
type Disk struct {
	root string
}

// NewDisk creates the cache directory if needed.
func NewDisk(root string) (*Disk, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Disk{root: root}, nil
}

// Key identifies one cache entry.
//
// SourceSize and SourceMod come from a stat, which only a local source has. An
// upstream source leaves them zero and fills SourceVersion instead, from the
// origin's ETag or Last-Modified — or, when the origin offers neither, from a
// coarse time bucket, which is how a TTL is expressed here. Putting the TTL in
// the key rather than in the entry means expiry needs no machinery: the key
// simply changes, the old entry stops being asked for, and the evictor collects
// it like any other cold entry.
type Key struct {
	SourcePath string
	SourceSize int64
	SourceMod  int64

	// SourceVersion is an opaque validator that changes when the source's bytes
	// change. Empty for a local source, whose identity size and mtime already
	// cover.
	SourceVersion string

	Chain string // the raw x-oss-process value
}

// Hash renders the key as a hex digest. It is exported because the HTTP layer
// uses the same string to key its singleflight group, so a cache miss and the
// in-flight call that fills it agree on identity.
//
// Lengths are written between the fields so that two different keys cannot
// serialise to the same bytes (path "a/b" + chain "c" vs path "a" + chain "b/c").
func (k Key) Hash() string {
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{0})
		h.Write([]byte(s))
	}
	write(k.SourcePath)
	write(strconv.FormatInt(k.SourceSize, 10))
	write(strconv.FormatInt(k.SourceMod, 10))
	// Only mixed in when there is one, so a local deployment's keys — and
	// therefore its warm cache — survive the arrival of upstream mode. The
	// length prefix already stops an empty version from colliding with anything.
	if k.SourceVersion != "" {
		write(k.SourceVersion)
	}
	write(k.Chain)
	return hex.EncodeToString(h.Sum(nil))
}

// path is the on-disk location, fanned out two levels so no single directory
// holds every entry.
func (d *Disk) path(k Key) string {
	s := k.Hash()
	return filepath.Join(d.root, s[:2], s[2:4], s[4:])
}

// Entry is a cached result opened for reading.
type Entry struct {
	*os.File
	Size int64
}

// Get opens the cached result for k, or returns ErrMiss.
func (d *Disk) Get(k Key) (*Entry, error) {
	f, err := os.Open(d.path(k))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrMiss
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Entry{File: f, Size: st.Size()}, nil
}

// Put stores b under k.
//
// A failure to cache is not a failure to serve: callers log and carry on.
func (d *Disk) Put(k Key, b []byte) error {
	name := d.path(k)
	dir := filepath.Dir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	if _, err := tmp.Write(b); err != nil {
		cleanup()
		return err
	}
	// Durability here is deliberately weak: an entry lost to a crash is
	// recomputed on the next request, so paying for fsync on every write would
	// buy nothing.
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, name); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// WriteTo copies a cached entry to w.
func (e *Entry) WriteTo(w io.Writer) (int64, error) { return io.Copy(w, e.File) }

// Stats walks the cache and reports entry count and total bytes. It is O(n) in
// entries and is meant for an admin endpoint, not a request path.
func (d *Disk) Stats() (entries int, bytes int64, err error) {
	err = filepath.Walk(d.root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		entries++
		bytes += info.Size()
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("cache: stats: %w", err)
	}
	return entries, bytes, nil
}
