//go:build unix

package main

import "syscall"

// inboxOpenFlags open an inbox file without blocking on a FIFO swapped in
// after the check. O_NOFOLLOW is passed too, though an os.Root follows a
// link that stays inside it either way; the same-file check after the open
// is what refuses one.
const inboxOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
