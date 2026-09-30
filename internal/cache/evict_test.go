package cache

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func fill(t *testing.T, d *Disk, n, size int) {
	t.Helper()
	blob := make([]byte, size)
	for i := 0; i < n; i++ {
		k := Key{SourcePath: fmt.Sprintf("/img%d.jpg", i), SourceSize: 1, SourceMod: 1, Chain: "c"}
		if err := d.Put(k, blob); err != nil {
			t.Fatal(err)
		}
		// Space the writes so access times are distinguishable.
		time.Sleep(2 * time.Millisecond)
	}
}

func totalBytes(t *testing.T, d *Disk) int64 {
	t.Helper()
	_, b, err := d.Stats()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEvictBySize(t *testing.T) {
	d, err := NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 10 entries of 1000 bytes = 10,000 total; a 6,000 limit should leave six.
	fill(t, d, 10, 1000)

	if got := totalBytes(t, d); got != 10000 {
		t.Fatalf("setup: %d bytes, want 10000", got)
	}

	removed, freed, err := d.Evict(EvictConfig{MaxBytes: 6000, Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	after := totalBytes(t, d)
	if after > 6000 {
		t.Errorf("after eviction: %d bytes, want <= 6000", after)
	}
	// The point of a size bound is to stop at the bound, not to empty the cache.
	if after == 0 {
		t.Errorf("eviction emptied the cache; removed=%d freed=%d", removed, freed)
	}
	if want := int64(6000); after < want-1000 {
		t.Errorf("evicted too much: %d bytes left, expected close to %d", after, want)
	}
}

func TestEvictUnderLimitDoesNothing(t *testing.T) {
	d, err := NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fill(t, d, 3, 1000)

	removed, _, err := d.Evict(EvictConfig{MaxBytes: 100000, Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("removed %d entries while under the limit", removed)
	}
}

func TestEvictByAge(t *testing.T) {
	dir := t.TempDir()
	d, err := NewDisk(dir)
	if err != nil {
		t.Fatal(err)
	}
	fill(t, d, 4, 100)

	// Backdate two entries past the age bound.
	old := time.Now().Add(-2 * time.Hour)
	n := 0
	walkFiles(t, dir, func(p string) {
		if n < 2 {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
			n++
		}
	})

	removed, _, err := d.Evict(EvictConfig{MaxAge: time.Hour, Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed %d entries, want 2", removed)
	}
	if entries, _, _ := d.Stats(); entries != 2 {
		t.Errorf("%d entries left, want 2", entries)
	}
}

func TestEvictDisabled(t *testing.T) {
	d, err := NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fill(t, d, 3, 1000)

	// No bounds set: nothing should go.
	removed, _, err := d.Evict(EvictConfig{Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("removed %d entries with no bound configured", removed)
	}
}

func walkFiles(t *testing.T, root string, fn func(string)) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		p := root + "/" + e.Name()
		if e.IsDir() {
			walkFiles(t, p, fn)
			continue
		}
		fn(p)
	}
}
