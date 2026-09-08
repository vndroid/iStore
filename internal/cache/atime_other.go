//go:build !linux && !darwin

package cache

import (
	"os"
	"time"
)

// atime falls back to the modification time where access times are not
// available through os.FileInfo, making eviction FIFO rather than LRU.
func atime(info os.FileInfo) time.Time { return info.ModTime() }
