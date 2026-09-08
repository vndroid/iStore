//go:build darwin

package cache

import (
	"os"
	"syscall"
	"time"
)

// Darwin exposes the access timestamp as Atimespec rather than Linux's Atim.
func atime(info os.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec)
	}
	return info.ModTime()
}
