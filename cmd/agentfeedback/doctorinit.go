package main

import (
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/pelletier/go-toml/v2"
)

const keyReadLimit = 64 << 10

// initOutcome is the one JSON line doctor --init always ends with.
type initOutcome struct {
	Status  string `json:"status"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message,omitempty"`
}

// printInitOutcome prints the outcome of doctor --init and returns err marked
// as already reported, so run adds nothing to stderr.
func printInitOutcome(stdout io.Writer, path string, err error) error {
	if err == nil {
		return writeJSON(stdout, initOutcome{Status: "written", Path: path})
	}
	if werr := writeJSON(stdout, initOutcome{Status: "error", Message: err.Error()}); werr != nil {
		return werr
	}

	return &reportedError{err}
}

// runDoctorInit writes url and api_key to the config file. It makes no
// network call. Without force an existing file is never touched: the new file
// is linked into place, which fails when the name exists. With force it is
// renamed over the old one. The file is 0600 from creation.
func runDoctorInit(getenv func(string) string, rawURL string, keyFromStdin, force bool, stdin io.Reader) (string, error) {
	if !keyFromStdin {
		return "", errInitNeedsStdin()
	}
	if err := checkInitURL(rawURL); err != nil {
		return "", err
	}
	key, err := readKey(stdin)
	if err != nil {
		return "", err
	}
	path, err := configPath(getenv)
	if err != nil {
		return "", err
	}

	data, err := toml.Marshal(struct {
		URL    string `toml:"url"`
		APIKey string `toml:"api_key"`
	}{rawURL, key})
	if err != nil {
		return "", errInitWrite(path, err)
	}

	return path, writeConfigFile(path, data, force)
}

func checkInitURL(raw string) error {
	if raw == "" {
		return errInitNeedsURL()
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return errInitBadURL(raw, "it does not parse")
	case u.Scheme != "http" && u.Scheme != "https":
		return errInitBadURL(raw, "the scheme is not http or https")
	case u.Host == "" || u.Hostname() == "":
		return errInitBadURL(raw, "it has no host")
	case u.User != nil:
		return errInitBadURL(raw, "it carries credentials")
	case u.RawQuery != "" || u.ForceQuery:
		return errInitBadURL(raw, "it has a query")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return errInitBadURL(raw, "it has a fragment")
	}

	return nil
}

// readKey reads one key from stdin: surrounding whitespace is trimmed, and
// anything left that is whitespace or a control character is refused.
func readKey(stdin io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(stdin, keyReadLimit+1))
	if err != nil {
		return "", errInitKeyRead(err)
	}
	if len(data) > keyReadLimit {
		return "", errInitKeyTooLong()
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", errInitKeyEmpty()
	}
	if strings.ContainsFunc(key, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", errInitKeyInvalid()
	}

	return key, nil
}

func writeConfigFile(path string, data []byte, force bool) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errInitWrite(path, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return errInitWrite(path, err)
	}
	tmpName := tmp.Name()
	// The temporary name never outlives this call: after a link it is a second
	// name for the file, after a rename it no longer exists.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return errInitWrite(path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()

		return errInitWrite(path, err)
	}
	if err := tmp.Close(); err != nil {
		return errInitWrite(path, err)
	}

	if force {
		if err := os.Rename(tmpName, path); err != nil {
			return errInitWrite(path, err)
		}

		return nil
	}
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errInitExists(path)
		}

		return errInitWrite(path, err)
	}

	return nil
}
