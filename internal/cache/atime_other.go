//go:build !linux && !darwin

package cache

import (
	"os"
	"time"
)

// atime falls back to the modification time where access times are not
// available through os.FileInfo, making eviction FIFO rather than LRU.
//
// This covers Windows and the BSDs. FreeBSD and NetBSD spell the field
// Atimespec like Darwin does, and OpenBSD and Dragonfly spell it Atim like
// Linux; rather than carry four variants for platforms nobody has tested the
// service on, they all land here and lose LRU. Only Linux is supported.
func atime(info os.FileInfo) time.Time { return info.ModTime() }
