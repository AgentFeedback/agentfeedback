//go:build !unix

package main

// inboxOpenFlags adds nothing where the platform has no O_NONBLOCK; the
// same-file check after the open still refuses an entry swapped in.
const inboxOpenFlags = 0
