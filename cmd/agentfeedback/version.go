package main

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
)

// version and commit are set at build time with
// -ldflags "-X main.version=... -X main.commit=...". When empty, the build
// information the Go toolchain embeds fills them in.
var version, commit string

type buildVersion struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Go      string `json:"go"`
}

// clientVersion returns the version of this binary: the ldflags values first,
// then the embedded build information.
func clientVersion() buildVersion {
	info, _ := debug.ReadBuildInfo()
	fbVersion, fbCommit := versionFromBuildInfo(info)
	v := buildVersion{Version: version, Commit: commit, Go: runtime.Version()}
	if v.Version == "" {
		v.Version = fbVersion
	}
	if v.Commit == "" {
		v.Commit = fbCommit
	}

	return v
}

// versionFromBuildInfo derives a version and commit from the toolchain's build
// information: "dev" for a (devel) or unversioned build, the VCS revision
// with "-dirty" when the tree was modified, "unknown" without one.
func versionFromBuildInfo(info *debug.BuildInfo) (ver, rev string) {
	ver, rev = "dev", "unknown"
	if info == nil {
		return ver, rev
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		ver = v
	}
	var revision string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision != "" {
		rev = revision
		if modified {
			rev += "-dirty"
		}
	}

	return ver, rev
}

func runVersion(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("version")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args, stderr); err != nil {
		return errFlags("version", err)
	}
	if fs.NArg() != 0 {
		return errArgs("version", "version [--json]")
	}

	v := clientVersion()
	if *asJSON {
		return writeJSON(stdout, v)
	}
	_, err := fmt.Fprintf(stdout, "agentfeedback %s (commit %s, %s)\n", v.Version, v.Commit, v.Go)

	return err
}

// writeJSON prints v as one line of JSON.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)

	return enc.Encode(v)
}
