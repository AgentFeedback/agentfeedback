package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strings"
)

// userError is the one shape of every error a person or an agent reads from
// this binary: what is wrong, then the next action, as one sentence.
// Usage errors (a wrong command line) exit 2; every other one exits 1.
type userError struct {
	problem string
	next    string
	usage   bool
}

func (e *userError) Error() string { return e.problem + "; " + e.next + "." }

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

func errLogLevel(got string) error {
	return failErr(fmt.Sprintf("LOG_LEVEL must be debug or info, got %q", got), "set LOG_LEVEL to debug or info")
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

func errURLUnset() error {
	return failErr("no server URL is set", "pass --url, set AGENT_FEEDBACK_URL, or run agentfeedback doctor --init")
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

func errInitKeyRead(err error) error {
	return failErr(fmt.Sprintf("cannot read the API key from stdin: %v", err), "pass the key on stdin to agentfeedback doctor --init --key-from-stdin")
}

func errInitKeyEmpty() error {
	return failErr("the API key read from stdin is empty", "pass the key on stdin to agentfeedback doctor --init --key-from-stdin")
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

func errSkillVerb(verb string) error {
	return usageErr(fmt.Sprintf("unknown skill subcommand %q; the only one is render", verb),
		"use agentfeedback "+skillSynopsis)
}

// errSkillServer never shows credentials: a URL that carries them is refused
// for that reason, and the refusal must not print them.
func errSkillServer(raw, reason string) error {
	shown := raw
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		shown = u.Redacted()
	} else if err != nil && strings.Contains(raw, "@") {
		shown = "(not shown)"
	}

	return usageErr(fmt.Sprintf("--server %q is not a usable base URL: %s", shown, reason),
		"pass an http or https URL without credentials, query or fragment")
}

// Read and processing command errors (list, get, stats, done, undo, redact,
// rekind, export, digest).

func errClientSetup(err error) error {
	return failErr(fmt.Sprintf("the client cannot be set up: %s", oneLine(err.Error())), "run agentfeedback doctor")
}

// errURLUnsetNoFlag is errURLUnset for the commands that take no --url.
func errURLUnsetNoFlag() error {
	return failErr("no server URL is set", "set AGENT_FEEDBACK_URL or run agentfeedback doctor --init")
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

func errWorkdir(err error) error {
	return failErr(fmt.Sprintf("cannot resolve the working directory (%v); narrowing rules cannot be applied, so nothing was sent", err),
		"run agentfeedback from a directory that exists")
}
