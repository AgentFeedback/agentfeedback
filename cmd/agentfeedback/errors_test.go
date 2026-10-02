package main

import (
	"errors"
	"strings"
	"testing"
)

// catalogue lists every user-facing error constructor with sample arguments;
// a new constructor is added here so its message is checked.
var catalogue = map[string]error{
	"errUnknownCommand":           errUnknownCommand("x"),
	"errFlags":                    errFlags("doctor", errors.New("flag provided but not defined: -x")),
	"errArgs":                     errArgs("backup", "backup <dest.db>"),
	"errAPIKeyUnset":              errAPIKeyUnset(),
	"errLogLevel":                 errLogLevel("trace"),
	"errDuration":                 errDuration("GRACEFUL_SHUTDOWN_TIMEOUT", "x", errors.New("bad")),
	"errDurationNeg":              errDuration("GRACEFUL_SHUTDOWN_TIMEOUT", "-1s", nil),
	"errDatabaseDir":              errDatabaseDir("/data", "is not writable"),
	"errBackupExists":             errBackupExists("/tmp/b.db"),
	"errBackupNoSource":           errBackupNoSource("/data/a.db"),
	"errImportRead":               errImportRead("/tmp/e.ndjson", errors.New("denied")),
	"errImportRejected":           errImportRejected("/tmp/e.ndjson", "line 7: trailer sha256 does not match"),
	"errImportFailed":             errImportFailed("/tmp/e.ndjson", "the database is busy"),
	"errBackupStat":               errBackupStat("/tmp/b.db", errors.New("denied")),
	"errConfigInvalid":            errConfigInvalid("/c.toml", errors.New("bad\ntoml")),
	"errConfigRead":               errConfigRead("/c.toml", errors.New("denied")),
	"errNoHome":                   errNoHome(errors.New("unset")),
	"errConfigMode":               errConfigMode("/c.toml", "0644"),
	"errURLUnset":                 errURLUnset(),
	"errKeyUnset":                 errKeyUnset(),
	"errKeyRejected":              errKeyRejected(401),
	"errMetaStatus":               errMetaStatus("http://x/api/v1/meta", 500),
	"errMetaBody":                 errMetaBody("http://x/api/v1/meta", "eof"),
	"errMetaRedirect":             errMetaRedirect("http://x/api/v1/meta", 301, "https://y/api/v1/meta"),
	"errURLCredentials":           errURLCredentials(),
	"errPathUnreadable":           errPathUnreadable("/c/spool", errors.New("denied")),
	"errLeadingFlag":              errLeadingFlag("--json"),
	"errUnreachable":              errUnreachable("http://x/api/v1/meta", errors.New("refused")),
	"errClientTooOld":             errClientTooOld("v1.0.0", "v2.0.0"),
	"errInitNeedsStdin":           errInitNeedsStdin(),
	"errInitOnlyFlags":            errInitOnlyFlags(),
	"errInitNeedsURL":             errInitNeedsURL(),
	"errInitBadURL":               errInitBadURL("ftp://x", "the scheme is not http or https"),
	"errInitKeyRead":              errInitKeyRead(errors.New("eof"), "doctor --init --key-from-stdin"),
	"errInitKeyEmpty":             errInitKeyEmpty("migrate --to-key-from-stdin"),
	"errInitKeyInvalid":           errInitKeyInvalid(),
	"errInitKeyTooLong":           errInitKeyTooLong(),
	"errInitExists":               errInitExists("/c.toml"),
	"errInitWrite":                errInitWrite("/c.toml", errors.New("denied")),
	"errSchemaKind":               errSchemaKind("x", []string{"envelope", "friction"}),
	"errSchemaVersion":            errSchemaVersion("friction", "9", []string{"1"}),
	"errSkillForm":                errSkillForm("x", []string{"skill-md", "prompt"}),
	"errSkillServer":              errSkillServer("ftp://x", "the scheme is not http or https"),
	"errSkillVerb":                errSkillVerb("list"),
	"errSkillNeedsOut":            errSkillNeedsOut("docs"),
	"errSkillServerNotApplicable": errSkillServerNotApplicable("docs"),
	"errSkillOutOnlyDirs":         errSkillOutOnlyDirs(),
	"errSkillOutNotEmpty":         errSkillOutNotEmpty("/tmp/docs"),
	"errSkillOutDir":              errSkillOutDir("/tmp/docs", errors.New("denied")),
	"errSkillOutWrite":            errSkillOutWrite("/tmp/docs/SKILL.md", errors.New("denied")),
	"errClientSetup":              errClientSetup(errors.New("the server URL has no host; set AGENT_FEEDBACK_URL")),
	"errKeyRefused":               errKeyRefused(401),
	"errNotFound":                 errNotFound(7),
	"errBadRequest":               errBadRequest("limit must be between 1 and 500, got 0"),
	"errHTTP":                     errHTTP(503, "busy", "r1"),
	"errBadResponse":              errBadResponse("list", errors.New("eof")),
	"errPagination":               errPagination("an empty page claims more rows"),
	"errExclusive":                errExclusive("--open", "--processed"),
	"errTimeFlag":                 errTimeFlag("since", "yesterday"),
	"errID":                       errID("x"),
	"errVerdictRequired":          errVerdictRequired(),
	"errRekindKind":               errRekindKind(" "),
	"errRekindRedacted":           errRekindRedacted(3),
	"errRekindSameKind":           errRekindSameKind(3, "note"),
	"errExportIncomplete":         errExportIncomplete("there is no trailer line"),
	"errDigestDir":                errDigestDir("/d", errors.New("denied")),
	"errDigestNotEmpty":           errDigestNotEmpty("/d"),
	"errDigestWrite":              errDigestWrite("/d/digest.md", errors.New("denied")),
	"errURLUnsetNoFlag":           errURLUnsetNoFlag(),
	"errRedirected":               errRedirected(302, "https://y/"),
	"errRedirectedNoLocation":     errRedirected(301, ""),
	"errTooLarge":                 errTooLarge(),
	"errTooManyIDs":               errTooManyIDs(501),
	"errRekindProcessed":          errRekindProcessed(3, "fixed"),
	"errRekindContextFull":        errRekindContextFull(3, 32),
	"errDigestSymlink":            errDigestSymlink("/d"),
	"errDigestContaminated":       errDigestContaminated(2),
	"errMigrateNeedsTo":           errMigrateNeedsTo(),
	"errMigrateNeedsStdin":        errMigrateNeedsStdin(),
	"errMigrateBadTo":             errMigrateBadTo("https://u:p@x", "it carries credentials"),
	"errMigrateLimit":             errMigrateLimit(0),
	"errMigrateSameServer":        errMigrateSameServer("https://x"),
	"errMigrateTargetKey":         errMigrateTargetKey("https://x", 401),
	"errMigrateTooOld":            errMigrateTooOld("https://x"),
	"errMigrateTarget":            errMigrateTarget("https://x", "HTTP 503: busy"),
	"errMigrateRecordTooLarge":    errMigrateRecordTooLarge("u-1", 1024),
	"errMigrateStopped":           errMigrateStopped("HTTP 503: busy", 3, 2, 1),
	"errAPIKeyBoth":               errAPIKeyBoth(),
	"errAPIKeyFile":               errAPIKeyFile("/k", "is empty"),
	"errE2EOnlyFlags":             errE2EOnlyFlags(),
	"errE2ECheck":                 errE2ECheck("list", "the row is missing"),
	"errServeInitOnlyFlags":       errServeInitOnlyFlags(),
	"errServeInitPort":            errServeInitPort(0),
	"errUnsafePath":               errUnsafePath("--dir", "/a b", "contains whitespace"),
	"errServeInitExists":          errServeInitExists("/s/api-key"),
	"errServeInitWrite":           errServeInitWrite("/s/api-key", errors.New("denied")),
	"errServeInitKey":             errServeInitKey(errors.New("no entropy")),
	"errServerVerb":               errServerVerb("start"),
	"errServerMode":               errServerMode(),
	"errServerImageFlag":          errServerImageFlag(),
	"errServerImageUnknown":       errServerImageUnknown("dev"),
	"errServeEnvMissing":          errServeEnvMissing("/s/serve.env"),
	"errServeEnvInvalid":          errServeEnvInvalid("/s/serve.env", "line 2: unknown key X"),
	"errServerRoot":               errServerRoot(),
	"errServerExists":             errServerExists("/u/agentfeedback.service"),
	"errServerWrite":              errServerWrite("/u/agentfeedback.service", errors.New("denied")),
	"errWindowsUnsupported":       errWindowsUnsupported("serve --init"),
	"errServeInitDB":              errServeInitDB("/d", "is a directory"),
	"errServerKeyFile":            errServerKeyFile("/k", "does not exist"),
	"errServerBinary":             errServerBinary(errors.New("unknown")),
}

var nextVerbs = []string{
	"run", "export", "pass", "set", "fix", "check", "chmod", "edit", "remove", "upgrade", "use", "start", "retry", "give",
}

func TestCatalogue(t *testing.T) {
	for name, err := range catalogue {
		t.Run(name, func(t *testing.T) {
			var ue *userError
			if !errors.As(err, &ue) {
				t.Fatalf("not a userError: %T", err)
			}
			if strings.TrimSpace(ue.problem) == "" {
				t.Fatal("empty problem")
			}
			verb, _, _ := strings.Cut(ue.next, " ")
			found := false
			for _, v := range nextVerbs {
				found = found || verb == v
			}
			if !found {
				t.Fatalf("next %q does not start with an allowed verb", ue.next)
			}
			if msg := err.Error(); !strings.HasSuffix(msg, ".") || strings.HasSuffix(msg, "..") || strings.Contains(msg, "\n") {
				t.Fatalf("message %q", msg)
			}
		})
	}
}
