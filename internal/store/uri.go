package store

import (
	"net/url"
	"path/filepath"
	"strings"
)

// FileURI is the driver's file: URI for the database at path with query:
// the path percent-escaped, so ?, # and % stay part of it (SQLite decodes
// %XX in a URI path; the driver splits the query at the first raw ?). An
// ordinary path opens the same file as before; the URI text is unchanged
// only for a path without characters that need escaping.
func FileURI(path string, query url.Values) string {
	return fileURI(path, filepath.VolumeName(path), query)
}

// fileURI is FileURI with the path's volume name given, so the Windows forms
// are testable everywhere. A path with a drive letter gets a leading slash,
// which SQLite's Windows VFS drops again: without it the colon would read
// as a URI scheme. A path beginning with // is written after an empty
// authority (file:////x), so SQLite reads it verbatim instead of taking
// its first segment for a host.
func fileURI(path, volume string, query url.Values) string {
	if len(volume) == 2 && volume[1] == ':' {
		path = "/" + path
	}
	u := &url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: query.Encode()}
	if strings.HasPrefix(path, "//") {
		u.OmitHost = false
	}

	return u.String()
}
