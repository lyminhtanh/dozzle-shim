//go:build !unix

package source

import "os"

// inodeOf has no inode concept on non-unix platforms; return 0 so rotation is
// detected by size-shrink alone.
func inodeOf(_ os.FileInfo) uint64 { return 0 }
