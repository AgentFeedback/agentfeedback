//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package harness

import "io"

// Lock takes no lock where install is not supported.
func Lock(Env, io.Writer, string) (func(), error) { return func() {}, nil }
