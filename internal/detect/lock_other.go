//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package detect

import "os"

// tryLock takes no lock where advisory locks are not available.
func tryLock(*os.File) bool { return true }
