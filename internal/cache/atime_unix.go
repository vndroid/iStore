//go:build linux

package cache

import (
	"os"
	"syscall"
	"time"
)

// atime returns the file's last access time, falling back to its modification
// time when the filesystem does not track access times.
//
// Access time is what makes this an LRU rather than a FIFO: a cache entry read
// on every page load should outlive one written yesterday and never touched.
// Filesystems mounted noatime (or relatime, which only updates once a day)
// degrade this to approximately-FIFO, which is still a correct cache — just a
// less efficient one.
func atime(info os.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Atim.Sec, st.Atim.Nsec)
	}
	return info.ModTime()
}
