//go:build !unix

package sessions

import "os"

func openCursorFile(root *os.Root, path string) (*os.File, error) {
	return root.Open(path)
}
