package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
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

	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
	"github.com/agentfeedback/agentfeedback/v4/internal/localmode"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

const (
	metaTimeout   = 5 * time.Second
	metaBodyLimit = 1 << 20
	logTailBytes  = 64 << 10
	logTailLines  = 5
)

type doctorReport struct {
	Status   string        `json:"status"`
	Problems []string      `json:"problems"`
	Mode     string        `json:"mode"`
	Database databaseCheck `json:"database"`
	Config   configCheck   `json:"config"`
	URL      resolvedValue `json:"url"`
	APIKey   keyCheck      `json:"api_key"`
	Meta     metaCheck     `json:"meta"`
	Versions versionCheck  `json:"versions"`
	Spool    spoolCheck    `json:"spool"`
	// LegacySpool counts the files of the spool a previous version kept in
	// the cache directory; anything there is a problem naming the remedy.
	LegacySpool spoolCheck     `json:"legacy_spool"`
	Recent      recentOutcomes `json:"recent"`
	// Harnesses are the binaries the CLI-mode harnesses' hooks run, and
	// Path the agentfeedback a shell finds, which the skill runs.
	Harnesses []harnessBinary `json:"harnesses"`
	Path      pathCheck       `json:"path"`
	Collect   collectCheck    `json:"collect"`
}

// harnessBinary is the binary one CLI-mode harness's hooks run, and when
// its hook last ran; cli is set for a CLI-mode harness with a hook.
type harnessBinary struct {
	Name    string `json:"name"`
	Binary  string `json:"binary"`
	Version string `json:"version"`
	// HookLastRun is the time the hook last ran, from the file it writes.
	HookLastRun string `json:"hook_last_run,omitempty"`
	cli         bool
}

// pathCheck is the agentfeedback on PATH, symlinks resolved.
type pathCheck struct {
	Binary  string `json:"binary,omitempty"`
	Version string `json:"version,omitempty"`
	Found   bool   `json:"found"`
}

// collectCheck lists the problems in the collection settings for the
// working directory; each is also a problem of the report.
type collectCheck struct {
	Warnings []string `json:"warnings"`
}

// databaseCheck is the local database: its path and whether it exists. Both
// are empty in remote mode. Loose lists the paths under the data directory
// (the directory, spool/, rejected/, the database files) whose mode lets
// other users in, in either mode; each is also a problem.
type databaseCheck struct {
	Path   string   `json:"path"`
	Exists bool     `json:"exists"`
	Loose  []string `json:"loose,omitempty"`
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
// JSON object with --json. --init hands over to runDoctorInit, --e2e to
// runDoctorE2E.
func runDoctor(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	// doctor delivers nothing, --e2e included: its one submission is its own.
	skipStartupPass = true
	fs := newFlagSet("doctor")
	urlFlag := fs.String("url", "", "server base URL (overrides AGENT_FEEDBACK_URL and the config file)")
	asJSON := fs.Bool("json", false, "print one JSON object")
	doInit := fs.Bool("init", false, "write the config file from --url and the key on stdin; no network call")
	keyFromStdin := fs.Bool("key-from-stdin", false, "with --init: read the API key from stdin")
	force := fs.Bool("force", false, "with --init: replace an existing config file")
	e2e := fs.Bool("e2e", false, "submit, list and mark one install-check row against the server; one line per step")
	mf := addModeFlags(fs)
	if err := parseFlags(fs, args, stderr); err != nil {
		err = errFlags("doctor", err)
		if slices.ContainsFunc(args, isInitArg) && !errors.Is(err, flag.ErrHelp) {
			return printInitOutcome(stdout, "", err)
		}

		return err
	}
	if *doInit && (mf.local || mf.server != "") {
		return printInitOutcome(stdout, "", errInitModeFlags())
	}
	if !*doInit && *urlFlag != "" {
		if mf.server != "" && mf.server != *urlFlag {
			return errURLServerDiffer()
		}
		if mf.local {
			return errURLLocal()
		}
		mf.server = *urlFlag
	}
	if *e2e {
		if *doInit || *keyFromStdin || *force {
			return errE2EOnlyFlags()
		}
		if fs.NArg() != 0 {
			return errArgs("doctor", "doctor --e2e [--url URL] [--json]")
		}

		return runDoctorE2E(os.Getenv, *mf, *asJSON, stdout, stderr)
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

	report, err := diagnose(os.Getenv, *mf, clientVersion().Version)
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

// diagnose runs every check. Only the /meta request touches the network (in
// local mode it is served in-process), and nothing is created or delivered:
// a missing local database is reported, not created, and the start-up pass
// does not run.
func diagnose(getenv func(string) string, mf modeFlags, clientVer string) (doctorReport, error) {
	r := doctorReport{Problems: []string{}, Recent: recentOutcomes{Lines: []json.RawMessage{}},
		Harnesses: []harnessBinary{}, Collect: collectCheck{Warnings: []string{}}}
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
	if exists && err == nil {
		checkConfigKeys(path, problem)
	}
	if cwd, err := os.Getwd(); err != nil {
		problem(errDoctorWorkdir(err))
	} else {
		for _, w := range collect.Lint(cwd, "", file.Collect) {
			r.Collect.Warnings = append(r.Collect.Warnings, w)
			problem(errCollectWarning(w))
		}
	}

	// The mode is resolved over the file already loaded: a config file that
	// does not parse is a problem to report, not the end of the report.
	mode, err := resolveModeFrom(mf, getenv, file)
	if err != nil {
		return r, err
	}
	r.Mode = mode.Mode
	settings := mode.Settings
	r.URL = settings.URL
	if r.URL.Value != "" {
		r.URL.Value = redactURL(r.URL.Value)
		if u, err := url.Parse(settings.URL.Value); err == nil && u.User != nil {
			problem(errURLCredentials())
		}
	}
	r.APIKey = keyCheck{Set: settings.APIKey.Value != "", Source: settings.APIKey.Source}

	switch {
	case mode.Mode == modeLocal:
		r.Database.Path = mode.Database
		r.Meta = checkLocal(mode, getenv, &r.Database, problem)
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

	r.Harnesses = checkHarnessBinaries(clientVer, problem)
	if p, ok := pathBinary(); ok {
		r.Path = pathCheck{Binary: p, Version: binaryVersion(p), Found: true}
	}

	cache, err := cacheDir(getenv)
	if err != nil {
		return r, err
	}
	data, err := dataDir(getenv)
	if err != nil {
		return r, err
	}
	r.Spool = countSpool(data, problem)
	r.Database.Loose = checkModes(data, problem)
	if filepath.Clean(cache) != filepath.Clean(data) {
		r.LegacySpool = countSpool(cache, problem)
		if n := r.LegacySpool.Pending + r.LegacySpool.Rejected; n > 0 {
			problem(errLegacySpool(cache, n, data))
		}
	}
	r.Recent = tailLog(client.LogPath(cache), settings.APIKey.Value, problem)

	r.Status = "ok"
	if len(r.Problems) > 0 {
		r.Status = "error"
	}

	return r, nil
}

// checkHarnessBinaries lists the binary each CLI-mode harness the install
// manifest records runs from its hooks, with its version. A missing binary
// is a problem, and so is one older than the running client: the newer
// binary migrates the local database and the older one then fails. Nothing
// is checked on Windows, where install does not run.
func checkHarnessBinaries(clientVer string, problem func(error)) []harnessBinary {
	out := []harnessBinary{}
	if goos == "windows" {
		return out
	}
	env, err := harnessEnv()
	if err != nil {
		problem(err)

		return out
	}
	st, err := env.Status()
	if err != nil {
		problem(errHarnessStatus(err))

		return out
	}
	cache, cacheErr := cacheDir(os.Getenv)
	hooked := map[string]bool{}
	for _, a := range harness.Adapters() {
		hooked[a.Name] = a.Hook.Format != "none"
	}
	for _, h := range st {
		if h.Binary == "" {
			continue
		}
		hb := harnessBinary{Name: h.Name, Binary: h.Binary, Version: h.BinaryVersion, cli: h.Mode == harness.ModeCLI && hooked[h.Name]}
		if hb.cli && cacheErr == nil {
			hb.HookLastRun = checkHookRun(cache, h.Name, problem)
		}
		out = append(out, hb)
		if h.BinaryVersion == "missing" {
			problem(errHarnessBinaryMissing(h.Name, h.Binary))

			continue
		}
		if h.BinaryVersion == "unknown" {
			problem(errHarnessBinaryUnknown(h.Name, h.Binary))

			continue
		}
		if cmp, ok := compareVersions(h.BinaryVersion, clientVer); ok && cmp < 0 {
			problem(errHarnessBinaryOld(h.Name, h.Binary, h.BinaryVersion, clientVer))
		}
	}
	if legacy, err := env.LegacyHooks(); err == nil {
		for _, n := range legacy {
			problem(errHookLegacy(n))
		}
	}

	return out
}

// checkHookRun reads when the harness's hook last ran, and reports as a
// problem a plugin's record of failing to start the hook that is newer than
// the last run.
func checkHookRun(cache, name string, problem func(error)) string {
	var last time.Time
	ts := ""
	if data, err := os.ReadFile(hookLastRunPath(cache, name)); err == nil {
		var r lastRun
		if json.Unmarshal(data, &r) == nil {
			if t, err := time.Parse(time.RFC3339, r.TS); err == nil {
				last, ts = t, r.TS
			}
		}
	}
	data, err := os.ReadFile(hookSpawnErrorPath(cache, name))
	if err != nil {
		return ts
	}
	var se struct {
		TS    string `json:"ts"`
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &se) != nil {
		return ts
	}
	at, err := time.Parse(time.RFC3339Nano, se.TS)
	if err != nil || (!last.IsZero() && !at.After(last)) {
		return ts
	}
	problem(errHookSpawn(name, oneLine(se.Error)))

	return ts
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

	return parseMeta(m, io.LimitReader(resp.Body, metaBodyLimit), shown, problem)
}

// checkLocal checks the local database: whether it exists and, when it does,
// what /api/v1/meta says when served in-process. Opening it checks the
// schema stamp. A missing database is not a problem and is not created.
func checkLocal(mode clientMode, getenv func(string) string, db *databaseCheck, problem func(error)) metaCheck {
	m := metaCheck{}
	err := localmode.Stat(mode.Database)
	if errors.Is(err, localmode.ErrNoDatabase) {
		return m
	}
	if err != nil {
		problem(errPathUnreadable(mode.Database, err))

		return m
	}
	db.Exists = true
	m.Checked = true
	c, err := openLocalClient(mode, getenv, io.Discard)
	if err != nil {
		if cache, cerr := cacheDir(getenv); cerr == nil {
			logSchemaTooNew(cache, err)
		}
		problem(err)

		return m
	}
	shown := "the local database " + mode.Database
	ctx, cancel := context.WithTimeout(context.Background(), metaTimeout)
	defer cancel()
	resp, err := c.Do(ctx, http.MethodGet, "/api/v1/meta", nil, nil)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) {
			m.HTTPStatus = apiErr.Status
			problem(errMetaStatus(shown, apiErr.Status))

			return m
		}
		problem(errUnreachable(shown, err))

		return m
	}
	m.HTTPStatus = resp.Status

	return parseMeta(m, bytes.NewReader(resp.Body), shown, problem)
}

// parseMeta reads a 2xx /api/v1/meta body into m.
func parseMeta(m metaCheck, body io.Reader, shown string, problem func(error)) metaCheck {
	var meta *struct {
		ServiceVersion string `json:"service_version"`
		APIVersion     string `json:"api_version"`
		Client         struct {
			MinVersion  string `json:"min_version"`
			LatestKnown string `json:"latest_known"`
		} `json:"client"`
	}
	if err := json.NewDecoder(body).Decode(&meta); err != nil {
		problem(errMetaBody(shown, err.Error()))

		return m
	}
	if meta == nil || meta.ServiceVersion == "" || meta.APIVersion == "" {
		problem(errMetaBody(shown, "it has no service_version or api_version"))

		return m
	}
	m.OK = true
	m.ServiceVersion = meta.ServiceVersion
	m.APIVersion = meta.APIVersion
	m.ClientMinVersion = meta.Client.MinVersion
	m.ClientLatestKnown = meta.Client.LatestKnown

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

// checkModes reports the paths under the data directory whose mode lets
// other users in: the directory itself, spool/, rejected/, the inbox with
// its done/ and rejected/, and the database files (the database, -wal and
// -shm), each that exists, as a problem with the chmod to run. It runs in both modes, since the spool is
// the only copy of a pending report in either. Nothing is changed: the
// client tightens what it creates and leaves what it finds. Modes mean
// nothing on Windows.
func checkModes(data string, problem func(error)) []string {
	if goos == "windows" {
		return nil
	}
	var loose []string
	check := func(path string, want os.FileMode) {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0o077 == 0 {
			return
		}
		loose = append(loose, path)
		problem(errLooseMode(path, fmt.Sprintf("%04o", info.Mode().Perm()), want))
	}
	check(data, 0o700)
	check(client.SpoolDir(data), 0o700)
	check(client.RejectedDir(data), 0o700)
	inbox := client.InboxDir(data)
	check(inbox, 0o700)
	check(filepath.Join(inbox, inboxDone), 0o700)
	check(filepath.Join(inbox, inboxRejected), 0o700)
	for _, p := range localmode.Sidecars(filepath.Join(data, "agentfeedback.db")) {
		check(p, 0o600)
	}

	return loose
}

// countSpool counts the client spool under root (the data directory, or the
// cache directory a previous version used): pending submissions are the
// regular files directly in spool/ (not the dot-prefixed temporary files,
// not the *.rejected ones); rejected ones are the *.rejected files in spool/
// plus the regular files in the sibling rejected/. A missing directory holds
// none.
func countSpool(root string, problem func(error)) spoolCheck {
	var s spoolCheck
	for _, name := range listFiles(client.SpoolDir(root), problem) {
		switch {
		case strings.HasPrefix(name, "."):
		case strings.HasSuffix(name, ".rejected"):
			s.Rejected++
		default:
			s.Pending++
		}
	}
	s.Rejected += len(listFiles(client.RejectedDir(root), problem))

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
	if r.Mode == modeLocal {
		db := "missing"
		if r.Database.Exists {
			db = "present"
		}
		fmt.Fprintf(w, "mode:     local (database %s, %s)\n", r.Database.Path, db)
	} else {
		fmt.Fprintf(w, "mode:     %s\n", orNone(r.Mode))
	}
	if r.Mode != modeLocal || r.URL.Value != "" {
		fmt.Fprintf(w, "url:      %s (source: %s)\n", orNone(r.URL.Value), orNone(r.URL.Source))
		key := "not set"
		if r.APIKey.Set {
			key = "set"
		}
		fmt.Fprintf(w, "api key:  %s (source: %s)\n", key, orNone(r.APIKey.Source))
	}
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
	if n := r.LegacySpool.Pending + r.LegacySpool.Rejected; n > 0 {
		fmt.Fprintf(w, "legacy:   %d file(s) in the cache directory spool of a previous version\n", n)
	}
	fmt.Fprintf(w, "recent:   %d outcome(s), %d invalid line(s), %d redacted line(s)\n",
		len(r.Recent.Lines), r.Recent.Invalid, r.Recent.Redacted)
	for _, line := range r.Recent.Lines {
		fmt.Fprintf(w, "          %s\n", line)
	}
	for _, h := range r.Harnesses {
		fmt.Fprintf(w, "harness:  %s %s (%s)\n", h.Name, h.Binary, orNone(h.Version))
		if h.cli {
			fmt.Fprintf(w, "hook:     %s last ran at %s\n", h.Name, cmp.Or(h.HookLastRun, "never"))
		}
	}
	if r.Path.Found {
		fmt.Fprintf(w, "path:     %s (%s)\n", r.Path.Binary, orNone(r.Path.Version))
	} else {
		fmt.Fprintln(w, "path:     no agentfeedback on PATH")
	}
	fmt.Fprintf(w, "collect:  %d warning(s)\n", len(r.Collect.Warnings))
	for _, p := range r.Problems {
		fmt.Fprintf(w, "problem:  %s\n", p)
	}
	fmt.Fprintf(w, "status:   %s\n", r.Status)
}

func errURLServerDiffer() error {
	return usageErr("--url and --server name different servers", "pass one of them")
}

func errURLLocal() error {
	return usageErr("--local and --url are both set", "pass one of them")
}

func errInitModeFlags() error {
	return usageErr("--init takes --url, not --local or --server", "pass --url URL with --init")
}
