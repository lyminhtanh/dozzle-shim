//go:build unix

package source

import (
	"os"
	"syscall"
)

// inodeOf returns the inode number for a file on unix systems, used to detect
// log rotation (a new file reusing the same name gets a different inode).
func inodeOf(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}
