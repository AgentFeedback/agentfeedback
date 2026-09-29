package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	metaTimeout   = 5 * time.Second
	metaBodyLimit = 1 << 20
	logTailBytes  = 64 << 10
	logTailLines  = 5
)

type doctorReport struct {
	Status   string         `json:"status"`
	Problems []string       `json:"problems"`
	Config   configCheck    `json:"config"`
	URL      resolvedValue  `json:"url"`
	APIKey   keyCheck       `json:"api_key"`
	Meta     metaCheck      `json:"meta"`
	Versions versionCheck   `json:"versions"`
	Spool    spoolCheck     `json:"spool"`
	Recent   recentOutcomes `json:"recent"`
}

type configCheck struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Mode   string `json:"mode,omitempty"`
}

// keyCheck reports whether a key is set and where from; never the key.
type keyCheck struct {
	Set    bool   `json:"set"`
	Source string `json:"source"`
}

type metaCheck struct {
	Checked           bool   `json:"checked"`
	OK                bool   `json:"ok"`
	HTTPStatus        int    `json:"http_status,omitempty"`
	ServiceVersion    string `json:"service_version,omitempty"`
	APIVersion        string `json:"api_version,omitempty"`
	ClientMinVersion  string `json:"client_min_version,omitempty"`
	ClientLatestKnown string `json:"client_latest_known,omitempty"`
}

type versionCheck struct {
	Client     string `json:"client"`
	MinVersion string `json:"min_version,omitempty"`
	// Compare is ok, too_old, or unknown (a dev build, or no min_version).
	Compare string `json:"compare"`
}

type spoolCheck struct {
	Pending  int `json:"pending"`
	Rejected int `json:"rejected"`
}

type recentOutcomes struct {
	Lines    []json.RawMessage `json:"lines"`
	Invalid  int               `json:"invalid"`
	Redacted int               `json:"redacted"`
}

// runDoctor checks the client setup and prints one line per check, or one
// JSON object with --json. --init hands over to runDoctorInit.
func runDoctor(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("doctor")
	urlFlag := fs.String("url", "", "server base URL (overrides AGENT_FEEDBACK_URL and the config file)")
	asJSON := fs.Bool("json", false, "print one JSON object")
	doInit := fs.Bool("init", false, "write the config file from --url and the key on stdin; no network call")
	keyFromStdin := fs.Bool("key-from-stdin", false, "with --init: read the API key from stdin")
	force := fs.Bool("force", false, "with --init: replace an existing config file")
	if err := parseFlags(fs, args, stderr); err != nil {
		err = errFlags("doctor", err)
		if slices.ContainsFunc(args, isInitArg) && !errors.Is(err, flag.ErrHelp) {
			return printInitOutcome(stdout, "", err)
		}

		return err
	}
	if *doInit {
		if fs.NArg() != 0 {
			return printInitOutcome(stdout, "", errArgs("doctor", "doctor --init --url URL --key-from-stdin [--force]"))
		}
		path, err := runDoctorInit(os.Getenv, *urlFlag, *keyFromStdin, *force, stdin)

		return printInitOutcome(stdout, path, err)
	}
	if fs.NArg() != 0 {
		return errArgs("doctor", "doctor [--url URL] [--json]")
	}
	if *keyFromStdin || *force {
		return errInitOnlyFlags()
	}

	report, err := diagnose(os.Getenv, *urlFlag, clientVersion().Version)
	if err != nil {
		return err
	}
	if *asJSON {
		if err := writeJSON(stdout, report); err != nil {
			return err
		}
	} else {
		printReport(stdout, report)
	}
	if report.Status != "ok" {
		return &reportedError{errors.New("doctor found problems")}
	}

	return nil
}

// isInitArg reports whether a raw argument asks for --init, so a command line
// that fails to parse still ends with the JSON outcome --init promises.
func isInitArg(a string) bool {
	return a == "-init" || a == "--init" || strings.HasPrefix(a, "-init=") || strings.HasPrefix(a, "--init=")
}

// redactURL is the only form in which doctor shows a URL: any userinfo is
// replaced, and a URL that does not parse is not echoed at all.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable URL>"
	}
	if u.User != nil {
		// A user part alone can be a token, so the whole userinfo goes.
		u.User = url.User("REDACTED")
	}

	return u.String()
}

// diagnose runs every check. Only the /meta request touches the network, and
// nothing is created on disk.
func diagnose(getenv func(string) string, urlFlag, clientVer string) (doctorReport, error) {
	r := doctorReport{Problems: []string{}, Recent: recentOutcomes{Lines: []json.RawMessage{}}}
	problem := func(err error) { r.Problems = append(r.Problems, err.Error()) }

	path, err := configPath(getenv)
	if err != nil {
		return r, err
	}
	r.Config.Path = path
	file, exists, err := loadFileConfig(path)
	r.Config.Exists = exists
	if err != nil {
		problem(err)
	}
	if info, statErr := os.Stat(path); statErr == nil {
		r.Config.Mode = fmt.Sprintf("%04o", info.Mode().Perm())
		if info.Mode().Perm()&0o077 != 0 {
			problem(errConfigMode(path, r.Config.Mode))
		}
	}

	settings := resolveClient(flagConfig{URL: urlFlag}, getenv, file)
	r.URL = settings.URL
	if r.URL.Value != "" {
		r.URL.Value = redactURL(r.URL.Value)
		if u, err := url.Parse(settings.URL.Value); err == nil && u.User != nil {
			problem(errURLCredentials())
		}
	}
	r.APIKey = keyCheck{Set: settings.APIKey.Value != "", Source: settings.APIKey.Source}

	switch {
	case settings.URL.Value == "":
		problem(errURLUnset())
	case settings.APIKey.Value == "":
		problem(errKeyUnset())
	default:
		r.Meta = checkMeta(settings.URL.Value, settings.APIKey.Value, clientVer, problem)
	}

	r.Versions = versionCheck{Client: clientVer, MinVersion: r.Meta.ClientMinVersion, Compare: "unknown"}
	if r.Meta.ClientMinVersion != "" {
		if cmp, ok := compareVersions(clientVer, r.Meta.ClientMinVersion); ok {
			r.Versions.Compare = "ok"
			if cmp < 0 {
				r.Versions.Compare = "too_old"
				problem(errClientTooOld(clientVer, r.Meta.ClientMinVersion))
			}
		}
	}

	cache, err := cacheDir(getenv)
	if err != nil {
		return r, err
	}
	r.Spool = countSpool(cache, problem)
	r.Recent = tailLog(filepath.Join(cache, "log", "client.jsonl"), settings.APIKey.Value, problem)

	r.Status = "ok"
	if len(r.Problems) > 0 {
		r.Status = "error"
	}

	return r, nil
}

// checkMeta fetches <url>/api/v1/meta with the key and reports what the
// server says about itself and the client versions it accepts. Redirects are
// not followed: the key is for the configured server only.
func checkMeta(base, key, clientVer string, problem func(error)) metaCheck {
	m := metaCheck{Checked: true}
	endpoint := strings.TrimRight(base, "/") + "/api/v1/meta"
	shown := redactURL(endpoint)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		problem(errUnreachable(shown, err))

		return m
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "agentfeedback/"+clientVer)

	client := &http.Client{
		Timeout:       metaTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		problem(errUnreachable(shown, err))

		return m
	}
	defer func() { _ = resp.Body.Close() }()
	m.HTTPStatus = resp.StatusCode

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		problem(errKeyRejected(resp.StatusCode))

		return m
	case resp.StatusCode >= 300 && resp.StatusCode <= 399:
		problem(errMetaRedirect(shown, resp.StatusCode, redactURL(resp.Header.Get("Location"))))

		return m
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		problem(errMetaStatus(shown, resp.StatusCode))

		return m
	}

	var body *struct {
		ServiceVersion string `json:"service_version"`
		APIVersion     string `json:"api_version"`
		Client         struct {
			MinVersion  string `json:"min_version"`
			LatestKnown string `json:"latest_known"`
		} `json:"client"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, metaBodyLimit)).Decode(&body); err != nil {
		problem(errMetaBody(shown, err.Error()))

		return m
	}
	if body == nil || body.ServiceVersion == "" || body.APIVersion == "" {
		problem(errMetaBody(shown, "it has no service_version or api_version"))

		return m
	}
	m.OK = true
	m.ServiceVersion = body.ServiceVersion
	m.APIVersion = body.APIVersion
	m.ClientMinVersion = body.Client.MinVersion
	m.ClientLatestKnown = body.Client.LatestKnown

	return m
}

// pseudoVersion matches the tail of a Go pseudo-version
// (v1.0.2-0.20260929105946-6e0e23d761c5), which the toolchain stamps on an
// untagged build: its MAJOR.MINOR.PATCH is not a release and compares as
// unknown.
var pseudoVersion = regexp.MustCompile(`[0-9]{14}-[0-9a-f]{12}$`)

// semver is a parsed MAJOR.MINOR.PATCH with its prerelease identifiers.
type semver struct {
	core [3]int
	pre  []string
}

// parseVersion reads MAJOR.MINOR.PATCH[-PRERELEASE][+BUILD] with an optional
// leading "v"; build metadata is ignored. A Go pseudo-version is unparseable.
func parseVersion(s string) (semver, bool) {
	var out semver
	s, _, _ = strings.Cut(s, "+")
	if pseudoVersion.MatchString(s) {
		return out, false
	}
	s = strings.TrimPrefix(s, "v")
	s, pre, hasPre := strings.Cut(s, "-")
	if hasPre {
		out.pre = strings.Split(pre, ".")
		if slices.Contains(out.pre, "") {
			return out, false
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out.core[i] = n
	}

	return out, true
}

// compareVersions returns -1, 0 or 1 by semver 2.0 precedence; ok is false
// when either is unparseable.
func compareVersions(a, b string) (int, bool) {
	va, okA := parseVersion(a)
	vb, okB := parseVersion(b)
	if !okA || !okB {
		return 0, false
	}
	if c := slices.Compare(va.core[:], vb.core[:]); c != 0 {
		return c, true
	}
	switch {
	case len(va.pre) == 0 && len(vb.pre) == 0:
		return 0, true
	case len(va.pre) == 0:
		return 1, true
	case len(vb.pre) == 0:
		return -1, true
	}
	for i := range min(len(va.pre), len(vb.pre)) {
		if c := comparePrerelease(va.pre[i], vb.pre[i]); c != 0 {
			return c, true
		}
	}

	return cmpInt(len(va.pre), len(vb.pre)), true
}

// comparePrerelease orders one identifier: numeric ones numerically and
// below alphanumeric ones, alphanumeric ones lexically.
func comparePrerelease(a, b string) int {
	na, errA := strconv.ParseUint(a, 10, 64)
	nb, errB := strconv.ParseUint(b, 10, 64)
	switch {
	case errA == nil && errB == nil:
		return cmpInt(int(min(na, 1<<62)), int(min(nb, 1<<62)))
	case errA == nil:
		return -1
	case errB == nil:
		return 1
	}

	return strings.Compare(a, b)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}

	return 0
}

// countSpool counts the client spool: pending submissions are the regular
// files directly in spool/ (not the dot-prefixed temporary files, not the
// *.rejected ones); rejected ones are the *.rejected files in spool/ plus the
// regular files in the sibling rejected/. A missing directory holds none.
func countSpool(cache string, problem func(error)) spoolCheck {
	var s spoolCheck
	for _, name := range listFiles(filepath.Join(cache, "spool"), problem) {
		switch {
		case strings.HasPrefix(name, "."):
		case strings.HasSuffix(name, ".rejected"):
			s.Rejected++
		default:
			s.Pending++
		}
	}
	s.Rejected += len(listFiles(filepath.Join(cache, "rejected"), problem))

	return s
}

// listFiles returns the names of the regular files directly in dir. A
// missing dir holds none; any other failure is a problem.
func listFiles(dir string, problem func(error)) []string {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		problem(errPathUnreadable(dir, err))

		return nil
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}

	return names
}

// tailLog returns the last lines of the client log, read from at most its
// final 64 KiB. A line cut by that window is dropped rather than counted as
// invalid, and a line holding the API key is dropped and counted as redacted.
// A missing log holds none; any other failure is a problem.
func tailLog(path, key string, problem func(error)) recentOutcomes {
	out := recentOutcomes{Lines: []json.RawMessage{}}
	data, err := readTail(path)
	if errors.Is(err, fs.ErrNotExist) {
		return out
	}
	if err != nil {
		problem(errPathUnreadable(path, err))

		return out
	}

	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 4096), logTailBytes+1)
	for sc.Scan() {
		if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
			lines = append(lines, bytes.Clone(line))
		}
	}
	if len(lines) > logTailLines {
		lines = lines[len(lines)-logTailLines:]
	}
	for _, line := range lines {
		switch {
		case key != "" && bytes.Contains(line, []byte(key)):
			out.Redacted++
		case json.Valid(line):
			out.Lines = append(out.Lines, json.RawMessage(line))
		default:
			out.Invalid++
		}
	}

	return out
}

// readTail reads at most the final logTailBytes of path, starting after the
// first newline when the window cuts into the file.
func readTail(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	offset := max(info.Size()-logTailBytes, 0)
	data := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(data, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if offset > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}

	return data, nil
}

func printReport(w io.Writer, r doctorReport) {
	orNone := func(s string) string {
		if s == "" {
			return "none"
		}

		return s
	}
	cfg := "missing"
	if r.Config.Exists {
		cfg = "present, mode " + orNone(r.Config.Mode)
	}
	fmt.Fprintf(w, "config:   %s (%s)\n", r.Config.Path, cfg)
	fmt.Fprintf(w, "url:      %s (source: %s)\n", orNone(r.URL.Value), orNone(r.URL.Source))
	key := "not set"
	if r.APIKey.Set {
		key = "set"
	}
	fmt.Fprintf(w, "api key:  %s (source: %s)\n", key, orNone(r.APIKey.Source))
	switch {
	case !r.Meta.Checked:
		fmt.Fprintln(w, "meta:     skipped")
	case r.Meta.OK:
		fmt.Fprintf(w, "meta:     ok (HTTP %d, service %s, api %s, client min %s, latest %s)\n", r.Meta.HTTPStatus,
			orNone(r.Meta.ServiceVersion), orNone(r.Meta.APIVersion),
			orNone(r.Meta.ClientMinVersion), orNone(r.Meta.ClientLatestKnown))
	case r.Meta.HTTPStatus != 0:
		fmt.Fprintf(w, "meta:     failed (HTTP %d)\n", r.Meta.HTTPStatus)
	default:
		fmt.Fprintln(w, "meta:     failed (no response)")
	}
	fmt.Fprintf(w, "version:  client %s, server minimum %s (%s)\n", r.Versions.Client, orNone(r.Versions.MinVersion), r.Versions.Compare)
	fmt.Fprintf(w, "spool:    %d pending, %d rejected\n", r.Spool.Pending, r.Spool.Rejected)
	fmt.Fprintf(w, "recent:   %d outcome(s), %d invalid line(s), %d redacted line(s)\n",
		len(r.Recent.Lines), r.Recent.Invalid, r.Recent.Redacted)
	for _, line := range r.Recent.Lines {
		fmt.Fprintf(w, "          %s\n", line)
	}
	for _, p := range r.Problems {
		fmt.Fprintf(w, "problem:  %s\n", p)
	}
	fmt.Fprintf(w, "status:   %s\n", r.Status)
}
