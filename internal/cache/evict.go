package cache

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// EvictConfig bounds how large the cache may grow.
type EvictConfig struct {
	// MaxBytes is the total size the cache may occupy. Zero disables the size
	// bound.
	MaxBytes int64
	// MaxAge removes entries untouched for longer than this. Zero disables the
	// age bound.
	MaxAge time.Duration
	// Interval is how often to sweep. Zero disables eviction entirely.
	Interval time.Duration
}

// entry is one cached file, as seen by the sweeper.
type entry struct {
	path string
	size int64
	used time.Time
}

// StartEvictor runs a sweep every cfg.Interval until stop is closed.
//
// Eviction is a background sweep rather than accounting on every write because
// the cache is content-addressed and written by rename: there is no index to
// keep, and a stat walk of a few thousand files costs less than maintaining one.
func (d *Disk) StartEvictor(cfg EvictConfig, stop <-chan struct{}) {
	if cfg.Interval <= 0 || (cfg.MaxBytes <= 0 && cfg.MaxAge <= 0) {
		return
	}

	go func() {
		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				removed, freed, err := d.Evict(cfg)
				if err != nil {
					slog.Warn("cache eviction failed", "error", err)
					continue
				}
				if removed > 0 {
					slog.Info("cache evicted", "entries", removed, "bytes", freed)
				}
			}
		}
	}()
}

// Evict performs one sweep and reports what it removed.
func (d *Disk) Evict(cfg EvictConfig) (removed int, freed int64, err error) {
	var entries []entry
	var total int64

	err = filepath.Walk(d.root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			// A file vanishing mid-walk is normal: another sweep or a failed
			// write cleaning up after itself. Keep going.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		entries = append(entries, entry{path: p, size: info.Size(), used: atime(info)})
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, 0, err
	}

	drop := func(e entry) {
		if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
			return
		}
		removed++
		freed += e.size
		total -= e.size
	}

	// Age bound first: it is absolute, and dropping stale entries may already
	// bring the total under the size bound.
	if cfg.MaxAge > 0 {
		cutoff := time.Now().Add(-cfg.MaxAge)
		kept := entries[:0]
		for _, e := range entries {
			if e.used.Before(cutoff) {
				drop(e)
				continue
			}
			kept = append(kept, e)
		}
		entries = kept
	}

	// Size bound: oldest-used first.
	if cfg.MaxBytes > 0 && total > cfg.MaxBytes {
		sort.Slice(entries, func(i, j int) bool { return entries[i].used.Before(entries[j].used) })
		for _, e := range entries {
			if total <= cfg.MaxBytes {
				break
			}
			drop(e)
		}
	}

	return removed, freed, nil
}
