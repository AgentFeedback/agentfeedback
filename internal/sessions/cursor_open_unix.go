//go:build unix

package sessions

import (
	"os"
	"syscall"
)

// A checked regular file may become a FIFO before open. Nonblocking mode lets
// the caller validate the opened descriptor without waiting for a FIFO writer.
func openCursorFile(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
