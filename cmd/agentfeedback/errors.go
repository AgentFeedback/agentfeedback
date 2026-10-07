package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/internal/skillgen"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

// userError is the one shape of every error a person or an agent reads from
// this binary: what is wrong, then the next action, as one sentence.
// Usage errors (a wrong command line) exit 2; every other one exits 1.
type userError struct {
	problem string
	next    string
	usage   bool
	cause   error
}

func (e *userError) Error() string { return e.problem + "; " + e.next + "." }

// Unwrap returns the underlying error the problem was built from, or nil, so
// errors.Is sees through the user-facing wording.
func (e *userError) Unwrap() error { return e.cause }

// exitCode maps an error returned by a command to the process exit status.
func exitCode(err error) int {
	var ue *userError
	if errors.As(err, &ue) && ue.usage {
		return 2
	}

	return 1
}

func usageErr(problem, next string) *userError {
	return &userError{problem: problem, next: next, usage: true}
}

func failErr(problem, next string) *userError {
	return &userError{problem: problem, next: next}
}

// Router and command-line errors.

func errUnknownCommand(name string) error {
	return usageErr(fmt.Sprintf("unknown command %q", name), "run agentfeedback help to list the commands")
}

// errFlags passes flag.ErrHelp through: -h is a request, not a mistake.
func errFlags(command string, err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return err
	}

	return usageErr(fmt.Sprintf("invalid arguments: %v", err), "run agentfeedback "+command+" -h for its usage")
}

func errLeadingFlag(arg string) error {
	return usageErr(fmt.Sprintf("%s comes before the command", arg), "run agentfeedback <command> --json, with the command first")
}

func errArgs(command, synopsis string) error {
	return usageErr(fmt.Sprintf("wrong number of arguments for %s", command), "use agentfeedback "+synopsis)
}

// Server configuration errors (serve, import, backup).

func errAPIKeyUnset() error {
	return failErr("API_KEY is not set", "export it before running agentfeedback serve")
}

func errAPIKeyBoth() error {
	return failErr("API_KEY and API_KEY_FILE are both set", "set only one of them")
}

// errAPIKeyFile names the key file, never its content.
func errAPIKeyFile(path, reason string) error {
	return failErr(fmt.Sprintf("API_KEY_FILE %s %s", path, reason),
		"fix the file so it holds exactly one key, or rerun agentfeedback serve --init --force")
}

func errLogLevel(got string) error {
	return failErr(fmt.Sprintf("LOG_LEVEL must be debug or info, got %q", got), "set LOG_LEVEL to debug or info")
}

// errPublicURL states why PUBLIC_URL was refused without repeating the
// value, which may carry credentials.
func errPublicURL(err error) error {
	reason := err.Error()
	var se *skillgen.ServerError
	if errors.As(err, &se) {
		reason = se.Reason
	}
	return failErr("PUBLIC_URL is not a usable base URL: "+reason, "set PUBLIC_URL to the server's public base URL, such as https://feedback.example.com, or unset it")
}

func errDuration(key, got string, err error) error {
	if err != nil {
		return failErr(fmt.Sprintf("%s is not a duration: %v", key, err), "set "+key+" to a positive duration such as 30s")
	}

	return failErr(fmt.Sprintf("%s must be positive, got %s", key, got), "set "+key+" to a positive duration such as 30s")
}

func errDatabaseDir(dir, reason string) error {
	return failErr(fmt.Sprintf("DATABASE_PATH directory %s %s", dir, reason),
		"set DATABASE_PATH to a file in an existing, writable directory")
}

func errBackupNoSource(path string) error {
	return failErr(fmt.Sprintf("database %s does not exist; nothing to back up", path),
		"set DATABASE_PATH to the database file to back up")
}

func errImportRead(path string, err error) error {
	return failErr(fmt.Sprintf("cannot read %s: %v", path, err), "check the export file exists and is readable")
}

func errImportRejected(path, message string) error {
	return failErr(fmt.Sprintf("%s was not imported, nothing was written: %s", path, message),
		"fix the export or export it again")
}

func errImportFailed(path, message string) error {
	return failErr(fmt.Sprintf("restoring %s failed, nothing was written: %s", path, message),
		"retry the import, stopping the service first if it holds the database")
}

func errBackupExists(dest string) error {
	return failErr(fmt.Sprintf("destination %s already exists", dest), "give a new destination path or remove the old file")
}

func errBackupStat(dest string, err error) error {
	return failErr(fmt.Sprintf("cannot check destination %s: %v", dest, err), "check the destination directory exists and is readable")
}

// Client configuration errors.

func errConfigInvalid(path string, err error) error {
	return failErr(fmt.Sprintf("config file %s is not valid: %s", path, oneLine(err.Error())),
		"fix the file or rerun agentfeedback doctor --init --force")
}

func errConfigRead(path string, err error) error {
	return failErr(fmt.Sprintf("cannot read config file %s: %v", path, err), "check its permissions or remove it")
}

func errNoHome(err error) error {
	return failErr(fmt.Sprintf("cannot find the home directory: %v", err), "set HOME or XDG_CONFIG_HOME and XDG_CACHE_HOME")
}

// doctor errors and problems.

func errConfigMode(path string, mode string) error {
	return failErr(fmt.Sprintf("config file %s has mode %s and is readable by other users", path, mode), "chmod 600 "+path)
}

func errKeyUnset() error {
	return failErr("no API key is set", "set AGENT_FEEDBACK_API_KEY or run agentfeedback doctor --init")
}

func errKeyRejected(status int) error {
	return failErr(fmt.Sprintf("the server rejected the API key (HTTP %d)", status),
		"check AGENT_FEEDBACK_API_KEY or rerun agentfeedback doctor --init --force")
}

func errMetaStatus(url string, status int) error {
	return failErr(fmt.Sprintf("GET %s returned HTTP %d", url, status),
		"check the URL points at an AgentFeedback v1 server")
}

func errMetaBody(url, reason string) error {
	return failErr(fmt.Sprintf("GET %s: the response is not AgentFeedback metadata: %s", url, reason),
		"check the URL points at an AgentFeedback v1 server")
}

func errMetaRedirect(url string, status int, location string) error {
	return failErr(fmt.Sprintf("GET %s: the server redirected (HTTP %d) to %s", url, status, location),
		"set the server URL to that address")
}

// errUnreachable names the URL once: a *url.Error already repeats it, so only
// its cause is kept.
func errUnreachable(shown string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}

	return failErr(fmt.Sprintf("cannot reach %s: %v", shown, err), "check the server is running and reachable")
}

func errURLCredentials() error {
	return failErr("the server URL carries credentials",
		"remove them from the URL and keep the key in AGENT_FEEDBACK_API_KEY or config.toml")
}

func errPathUnreadable(path string, err error) error {
	return failErr(fmt.Sprintf("cannot read %s: %v", path, err), "check its permissions")
}

// errLooseMode: a data directory or database file other users can reach.
func errLooseMode(path, mode string, want os.FileMode) error {
	return failErr(fmt.Sprintf("%s has mode %s and is reachable by other users", path, mode), fmt.Sprintf("chmod %o %s", want, path))
}

// errLegacySpool: files left in the spool a previous version kept under the
// cache directory; nothing reads them any more.
func errLegacySpool(cache string, n int, data string) error {
	return failErr(fmt.Sprintf("the cache directory %s holds %d spool file(s) written by a previous version; the spool now lives in %s", cache, n, data),
		fmt.Sprintf("remove %s and %s, after sending them with the previous binary's flush if they matter", client.SpoolDir(cache), client.RejectedDir(cache)))
}

func errClientTooOld(have, minimum string) error {
	return failErr(fmt.Sprintf("this client is %s but the server requires at least %s", have, minimum),
		"upgrade the agentfeedback binary")
}

// doctor --init errors.

func errInitNeedsStdin() error {
	return usageErr("--init reads the API key from stdin only", "pass --key-from-stdin and pipe the key in")
}

func errInitOnlyFlags() error {
	return usageErr("--key-from-stdin and --force only apply to --init", "pass --init to write the config file")
}

func errInitNeedsURL() error {
	return usageErr("--init needs the server URL", "pass --url https://your-server")
}

func errInitBadURL(raw, reason string) error {
	return usageErr(fmt.Sprintf("--url %q is not usable: %s", raw, reason),
		"pass an http or https URL with a host and no credentials, query or fragment")
}

func errInitKeyRead(err error, command string) error {
	return failErr(fmt.Sprintf("cannot read the API key from stdin: %v", err), "pass the key on stdin to agentfeedback "+command)
}

func errInitKeyEmpty(command string) error {
	return failErr("the API key read from stdin is empty", "pass the key on stdin to agentfeedback "+command)
}

func errInitKeyInvalid() error {
	return failErr("the API key read from stdin contains whitespace or control characters",
		"pass exactly one key on stdin with nothing around it")
}

func errInitKeyTooLong() error {
	return failErr("the API key read from stdin is longer than 64 KiB", "pass exactly one key on stdin with nothing around it")
}

func errInitExists(path string) error {
	return failErr(fmt.Sprintf("config already exists at %s", path), "pass --force to replace it, or edit it by hand")
}

func errInitWrite(path string, err error) error {
	return failErr(fmt.Sprintf("cannot write config file %s: %v", path, err), "check the directory is writable")
}

// doctor --e2e errors.

func errE2EOnlyFlags() error {
	return usageErr("--e2e cannot be combined with --init, --key-from-stdin or --force", "run agentfeedback doctor --e2e on its own")
}

func errE2ECheck(step, reason string) error {
	return failErr(fmt.Sprintf("the install check failed at %s: %s", step, reason), "run agentfeedback doctor to check the setup")
}

// serve --init and server install errors.

func errServeInitOnlyFlags() error {
	return usageErr("--dir, --db, --port and --force only apply to --init", "pass --init to write the server files, or run agentfeedback serve alone")
}

func errServeInitPort(port int) error {
	return usageErr(fmt.Sprintf("--port %d is not a TCP port", port), "pass a port from 1 to 65535")
}

// errUnsafePath exists because the server files are written without escaping.
func errUnsafePath(what, path, reason string) error {
	return failErr(fmt.Sprintf("%s %q %s, which the server files cannot hold unescaped", what, path, reason),
		"use a path made of letters, digits and / . _ - + @ , = only")
}

func errWindowsUnsupported(command string) error {
	return usageErr(fmt.Sprintf("%s is not supported on Windows", command),
		"set API_KEY_FILE, DATABASE_PATH and HTTP_LISTEN_ADDR for agentfeedback serve yourself")
}

func errServeInitDB(path, reason string) error {
	return usageErr(fmt.Sprintf("--db %s %s", path, reason), "pass --db with a database file path of its own")
}

func errServerKeyFile(path, reason string) error {
	return failErr(fmt.Sprintf("API_KEY_FILE %s %s", path, reason), "run agentfeedback serve --init first")
}

func errServeInitExists(path string) error {
	return failErr(fmt.Sprintf("%s already exists", path),
		"pass --force to replace the server files, which rotates the API key")
}

func errServeInitWrite(path string, err error) error {
	return failErr(fmt.Sprintf("cannot write %s: %v", path, err), "check the directory is writable")
}

func errServeInitKey(err error) error {
	return failErr(fmt.Sprintf("cannot generate the API key: %v", err), "run agentfeedback serve --init again")
}

func errServerVerb(verb string) error {
	return usageErr(fmt.Sprintf("unknown server subcommand %q; the only one is install", verb), "use agentfeedback "+serverSynopsis)
}

func errServerMode() error {
	return usageErr("server install needs exactly one of --systemd, --launchd or --compose", "use agentfeedback "+serverSynopsis)
}

func errServerImageFlag() error {
	return usageErr("--image only applies to --compose", "pass --compose with --image")
}

func errServerImageUnknown(version string) error {
	return failErr(fmt.Sprintf("this build (%s) is not a release, so there is no image tag to use", version),
		"pass --image with the image reference to run")
}

func errServeEnvMissing(path string) error {
	return failErr(fmt.Sprintf("%s does not exist", path), "run agentfeedback serve --init first")
}

func errServeEnvInvalid(path, reason string) error {
	return failErr(fmt.Sprintf("%s is not valid: %s", path, reason), "fix the file or run agentfeedback serve --init --force")
}

func errServerRoot() error {
	return failErr("server install --systemd writes a user unit and refuses to run as root",
		"run it as the user the service runs as")
}

func errServerExists(path string) error {
	return failErr(fmt.Sprintf("%s already exists", path), "pass --force to replace it")
}

func errServerWrite(path string, err error) error {
	return failErr(fmt.Sprintf("cannot write %s: %v", path, err), "check the directory is writable")
}

func errServerBinary(err error) error {
	return failErr(fmt.Sprintf("cannot resolve the agentfeedback binary path: %v", err), "run agentfeedback from its installed path")
}

// schema errors.

func errSchemaKind(kind string, known []string) error {
	return usageErr(fmt.Sprintf("no schema of kind %q; the kinds are %s", kind, strings.Join(known, ", ")),
		"run agentfeedback schema to list them")
}

func errSchemaVersion(kind, version string, known []string) error {
	return usageErr(fmt.Sprintf("no version %s of schema %q; its versions are %s", version, kind, strings.Join(known, ", ")),
		"run agentfeedback schema to list them")
}

// skill errors.

func errSkillForm(form string, known []string) error {
	return usageErr(fmt.Sprintf("no skill form %q; the forms are %s", form, strings.Join(known, ", ")),
		"run agentfeedback skill -h to list them")
}

func errSkillNeedsOut(form string) error {
	return usageErr("skill render "+form+" writes a directory and needs --out", "pass --out with a new or empty directory")
}

func errSkillServerNotApplicable(form string) error {
	return usageErr("--server does not apply to skill render "+form, "remove --server and run it again")
}

func errSkillOutOnlyDirs() error {
	return usageErr("--out only applies to skill render docs, agent-plugin and marketplace; the other forms print on stdout", "remove --out and redirect stdout to a file")
}

func errSkillOutNotEmpty(dir string) error {
	return failErr(fmt.Sprintf("--out %s exists and is not an empty directory", dir), "give a new or empty --out directory")
}

func errSkillOutDir(dir string, err error) error {
	return failErr(fmt.Sprintf("cannot create %s: %v", dir, err), "check the directory is writable")
}

func errSkillOutWrite(path string, err error) error {
	return failErr(fmt.Sprintf("cannot write %s: %v", path, err), "check the directory is writable")
}

func errSkillVerb(verb string) error {
	return usageErr(fmt.Sprintf("unknown skill subcommand %q; the subcommands are render and reminder", verb),
		"use agentfeedback "+skillSynopsis)
}

// errSkillServer never shows credentials: a URL that carries them is refused
// for that reason, and the refusal must not print them.
func errSkillServer(raw, reason string) error {
	shown := shownURL(raw)

	return usageErr(fmt.Sprintf("--server %q is not a usable base URL: %s", shown, reason),
		"pass an http or https URL without credentials, query or fragment")
}

// shownURL is raw with its whole userinfo, user name included (it can be a
// key), replaced by REDACTED; a URL that does not parse but holds an @ is
// not shown at all.
func shownURL(raw string) string {
	u, err := url.Parse(raw)
	switch {
	case err == nil && u.User != nil:
		u.User = url.User("REDACTED")

		return u.String()
	case err != nil && strings.Contains(raw, "@"):
		return "(not shown)"
	}

	return raw
}

// Read and processing command errors (list, get, stats, done, undo, redact,
// rekind, export, digest).

func errClientSetup(err error) error {
	return failErr(fmt.Sprintf("the client cannot be set up: %s", oneLine(err.Error())), "run agentfeedback doctor")
}

func errRedirected(status int, location string) error {
	if location == "" {
		location = "(no Location header)"
	}

	return failErr(fmt.Sprintf("the server redirected (HTTP %d) to %s", status, location),
		"set AGENT_FEEDBACK_URL to the address it redirects to, or check AGENT_FEEDBACK_URL")
}

func errTooLarge() error {
	return failErr("the server's answer is larger than 48 MiB", "pass a lower --limit")
}

func errTooManyIDs(n int) error {
	return usageErr(fmt.Sprintf("%d ids given; one batch marks at most 500", n), "pass at most 500 ids per run")
}

func errRekindProcessed(id int64, verdict string) error {
	return failErr(fmt.Sprintf("submission %d is already marked %s", id, verdict),
		fmt.Sprintf("run agentfeedback undo %d first to re-kind it", id))
}

func errRekindContextFull(id int64, n int) error {
	return failErr(fmt.Sprintf("submission %d has %d context entries, and the rekinded_from marker would overflow the 32-entry context limit", id, n),
		fmt.Sprintf("use agentfeedback get %d and submit a corrected copy by hand instead", id))
}

func errDigestSymlink(dir string) error {
	return failErr(fmt.Sprintf("--out %s is a symbolic link", dir), "give a real directory path for --out")
}

func errKeyRefused(status int) error {
	return failErr(fmt.Sprintf("the server refused the API key (HTTP %d)", status), "run agentfeedback doctor")
}

func errNotFound(id int64) error {
	return failErr(fmt.Sprintf("submission %d not found", id), "run agentfeedback list to find its id")
}

func errBadRequest(message string) error {
	return failErr(fmt.Sprintf("the server rejected the request: %s", oneLine(message)), "fix the arguments and run the command again")
}

func errHTTP(status int, message, requestID string) error {
	return failErr(fmt.Sprintf("HTTP %d: %s (request id %s)", status, oneLine(message), requestID),
		"retry the command, and run agentfeedback doctor if it fails again")
}

func errBadResponse(what string, err error) error {
	return failErr(fmt.Sprintf("the server's answer to %s is not readable: %v", what, err),
		"run agentfeedback doctor to check the URL points at an AgentFeedback v1 server")
}

func errPagination(reason string) error {
	return failErr(fmt.Sprintf("the server's pagination is inconsistent: %s", reason),
		"retry the command, and run agentfeedback doctor if it fails again")
}

func errExclusive(a, b string) error {
	return usageErr(fmt.Sprintf("%s and %s cannot be combined", a, b), "pass only one of them")
}

func errTimeFlag(name, got string) error {
	return usageErr(fmt.Sprintf("--%s %q is neither an RFC 3339 time nor a relative time", name, got),
		"pass a time like 2026-09-30T12:00:00Z or 30m, 12h, 7d, 2w")
}

func errID(arg string) error {
	return usageErr(fmt.Sprintf("%q is not a submission id", arg), "pass positive integer ids")
}

func errVerdictRequired() error {
	return usageErr("done needs a verdict",
		"pass --verdict fixed, invalid, duplicate, wont_fix, deferred, upstream or unverifiable")
}

func errRekindKind(got string) error {
	return usageErr(fmt.Sprintf("%q is not a kind", got), "pass a kind such as friction or review")
}

func errRekindRedacted(id int64) error {
	return failErr(fmt.Sprintf("submission %d is redacted and has no content to re-kind", id), "give the id of a row that is not redacted")
}

func errRekindSameKind(id int64, kind string) error {
	return failErr(fmt.Sprintf("submission %d is already of kind %s", id, kind), "pass a different kind")
}

func errExportIncomplete(reason string) error {
	return failErr(fmt.Sprintf("the export is incomplete: %s", reason), "run agentfeedback export again")
}

func errDigestDir(dir string, err error) error {
	return failErr(fmt.Sprintf("cannot create the digest directory %s: %v", dir, err), "check the directory is writable")
}

func errDigestNotEmpty(dir string) error {
	return failErr(fmt.Sprintf("--out directory %s is not empty, and digests are never mixed", dir),
		"give a new or empty --out directory")
}

func errDigestWrite(path string, err error) error {
	return failErr(fmt.Sprintf("cannot write %s: %v", path, err), "check the directory is writable")
}

// errDigestContaminated exits 2: the pull is not the queue it claims to be.
func errDigestContaminated(n int) error {
	return usageErr(fmt.Sprintf("%d pulled row(s) already have processed_at set, so the pull is contaminated", n),
		"run agentfeedback digest again before trusting it")
}

// oneLine keeps a multi-line library message on one line of output.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// submit and flush errors.

func errSubmitKind(got string) error {
	return usageErr(fmt.Sprintf("%q is not a kind", got), "use agentfeedback "+submitSynopsis)
}

func errStdinRead(err error) error {
	return failErr(fmt.Sprintf("cannot read stdin: %v", err), "pipe one JSON object to agentfeedback submit --stdin")
}

func errStdinObject(reason string) error {
	return usageErr(fmt.Sprintf("--stdin needs exactly one JSON object: %s", reason), "pipe one JSON object and nothing else")
}

func errSchemaVersionFlag(got string) error {
	return usageErr(fmt.Sprintf("--schema-version %q is not a positive integer", got), "pass a version such as 1")
}

func errPayloadNotObject() error {
	return usageErr("the payload given on stdin is not a JSON object, so a payload flag cannot be added to it",
		"drop the payload flags or send payload as an object")
}

func errSummaryRequired() error {
	return usageErr("submit friction needs a summary", "pass --summary or a summary member on stdin")
}

// migrate errors. The target's key is the key read from stdin; no message
// names AGENT_FEEDBACK_API_KEY for it, and none shows either key.

func errMigrateNeedsTo() error {
	return usageErr("migrate needs a target", "pass --to cloud or --to https://your-server")
}

func errMigrateNeedsStdin() error {
	return usageErr("migrate reads the target's API key from stdin only", "pass --to-key-from-stdin and pipe the key in")
}

// errMigrateBadTo never shows credentials, as errSkillServer.
func errMigrateBadTo(raw, reason string) error {
	shown := shownURL(raw)

	return usageErr(fmt.Sprintf("--to %q is not usable: %s", shown, reason),
		"pass cloud or an http or https URL with a host and no credentials, query or fragment")
}

func errMigrateLimit(n int) error {
	return usageErr(fmt.Sprintf("--limit %d is not a positive number of records", n), "pass --limit 1 or more")
}

func errMigrateSameServer(target string) error {
	return usageErr(fmt.Sprintf("the target %s is the configured source server", target), "pass another server with --to")
}

func errMigrateTargetKey(target string, status int) error {
	return failErr(fmt.Sprintf("the target %s refused the key read from stdin (HTTP %d)", target, status),
		"pass the target's API key on stdin to agentfeedback migrate --to-key-from-stdin")
}

func errMigrateTooOld(target string) error {
	return failErr(fmt.Sprintf("the target %s is too old: it has no POST /api/v1/import", target),
		"upgrade the target server or pass another --to")
}

func errMigrateTarget(target, reason string) error {
	return failErr(fmt.Sprintf("the target %s failed: %s", target, oneLine(reason)),
		"check the target URL points at an AgentFeedback v1 server and is reachable")
}

func errMigrateRecordTooLarge(uid string, limit int) error {
	return failErr(fmt.Sprintf("record uid=%s alone exceeds the target's %d-byte import limit", clean(uid), limit),
		"export it with agentfeedback export and move it by hand, or pass filters that leave it out")
}

// errMigrateStopped carries the counts of a run that stopped part way.
func errMigrateStopped(reason string, sent, imported, skipped int) error {
	return failErr(fmt.Sprintf("migration stopped after %d records sent (%d imported, %d skipped): %s",
		sent, imported, skipped, oneLine(reason)),
		"run agentfeedback migrate again once that is fixed, which is safe because the target skips the uids it already holds")
}

func errWorkdir(err error) error {
	return failErr(fmt.Sprintf("cannot resolve the working directory (%v); narrowing rules cannot be applied, so nothing was sent", err),
		"run agentfeedback from a directory that exists")
}

// install and uninstall errors.

// errInstallWindows refuses install and uninstall; install prints the
// wiring by hand before it.
func errInstallWindows(command string) error {
	next := "use the steps printed above to wire the harness by hand"
	if command == "uninstall" {
		next = "remove by hand what was wired by hand"
	}

	return usageErr(fmt.Sprintf("%s is not supported on Windows", command), next)
}

func errInstallHarness(name string, known []string) error {
	return usageErr(fmt.Sprintf("unknown harness %q; the harnesses are %s", name, strings.Join(known, ", ")),
		"pass all or one of those names")
}

func errInstallNoneDetected() error {
	return failErr("no harness was detected on this machine",
		"name the harnesses to wire, such as agentfeedback install claude-code")
}

func errInstallNoServer() error {
	return failErr("no server was named", "answer local, cloud or a URL, or pass --server")
}

// errInstallBadServer never shows credentials, as errSkillServer. A bad
// --server is a command-line error; a bad configured, recorded or typed
// server is not.
func errInstallBadServer(source, raw, reason string) error {
	problem := fmt.Sprintf("%s %q is not usable: %s", source, shownURL(raw), reason)
	next := "use cloud or an http or https URL with a host and no credentials, query, fragment or shell-unsafe characters"
	if source == "--server" {
		return usageErr(problem, next)
	}

	return failErr(problem, next)
}

func errInstallServerDiffers(flagURL, configured, source string) error {
	return failErr(fmt.Sprintf("--server %s differs from %s (%s), and install never changes the configured server", flagURL, configured, source),
		"drop --server, or change the server with agentfeedback doctor --init --force first (or unset the variable)")
}

// doctor checks of the harness binaries and the collection settings.

func errHarnessStatus(err error) error {
	return failErr(fmt.Sprintf("cannot read the install manifest: %s", oneLine(err.Error())),
		"fix or restore it, then run agentfeedback install --list")
}

func errHarnessBinaryMissing(name, bin string) error {
	return failErr(fmt.Sprintf("the %s hooks run %s, which does not exist, so they fail silently", name, bin),
		"run agentfeedback install "+name+" from the binary to keep")
}

func errHarnessBinaryUnknown(name, bin string) error {
	return failErr(fmt.Sprintf("the %s hooks run %s, which does not answer agentfeedback version --json, so they may fail silently", name, bin),
		"run agentfeedback install "+name+" from the binary to keep")
}

func errHarnessBinaryOld(name, bin, have, client string) error {
	return failErr(fmt.Sprintf("the %s hooks run %s version %s, older than this client %s; the newer binary migrates the local database and the older one then fails", name, bin, have, client),
		"run agentfeedback install "+name+" from the binary to keep")
}

func errConfigUnknownKey(path, key string) error {
	return failErr(fmt.Sprintf("%s: unknown key %s is ignored", path, key),
		"check its spelling (narrowing it names is not applied)")
}

func errCollectWarning(w string) error {
	return failErr(w, "fix the setting it names")
}

func errDoctorWorkdir(err error) error {
	return failErr(fmt.Sprintf("cannot resolve the working directory (%v); the collection settings were not checked", err),
		"run agentfeedback doctor from a directory that exists")
}
