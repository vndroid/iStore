//go:build darwin

package cache

import (
	"os"
	"syscall"
	"time"
)

// Darwin exposes the access timestamp as Atimespec rather than Linux's Atim.
//
// macOS is not a supported target — see README — but a wrong build tag here
// stops the package compiling at all, which would make the repository
// unopenable on a Mac for no gain. This exists so `go build` works while
// someone is writing code, not as a claim that the service runs there.
func atime(info os.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec)
	}
	return info.ModTime()
}
