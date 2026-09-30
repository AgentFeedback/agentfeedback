package main

import (
	"errors"
	"strings"
	"testing"
)

// catalogue lists every user-facing error constructor with sample arguments;
// a new constructor is added here so its message is checked.
var catalogue = map[string]error{
	"errUnknownCommand": errUnknownCommand("x"),
	"errFlags":          errFlags("doctor", errors.New("flag provided but not defined: -x")),
	"errArgs":           errArgs("backup", "backup <dest.db>"),
	"errAPIKeyUnset":    errAPIKeyUnset(),
	"errLogLevel":       errLogLevel("trace"),
	"errDuration":       errDuration("GRACEFUL_SHUTDOWN_TIMEOUT", "x", errors.New("bad")),
	"errDurationNeg":    errDuration("GRACEFUL_SHUTDOWN_TIMEOUT", "-1s", nil),
	"errDatabaseDir":    errDatabaseDir("/data", "is not writable"),
	"errBackupExists":   errBackupExists("/tmp/b.db"),
	"errBackupNoSource": errBackupNoSource("/data/a.db"),
	"errImportRead":     errImportRead("/tmp/e.ndjson", errors.New("denied")),
	"errImportRejected": errImportRejected("/tmp/e.ndjson", "line 7: trailer sha256 does not match"),
	"errImportFailed":   errImportFailed("/tmp/e.ndjson", "the database is busy"),
	"errBackupStat":     errBackupStat("/tmp/b.db", errors.New("denied")),
	"errConfigInvalid":  errConfigInvalid("/c.toml", errors.New("bad\ntoml")),
	"errConfigRead":     errConfigRead("/c.toml", errors.New("denied")),
	"errNoHome":         errNoHome(errors.New("unset")),
	"errConfigMode":     errConfigMode("/c.toml", "0644"),
	"errURLUnset":       errURLUnset(),
	"errKeyUnset":       errKeyUnset(),
	"errKeyRejected":    errKeyRejected(401),
	"errMetaStatus":     errMetaStatus("http://x/api/v1/meta", 500),
	"errMetaBody":       errMetaBody("http://x/api/v1/meta", "eof"),
	"errMetaRedirect":   errMetaRedirect("http://x/api/v1/meta", 301, "https://y/api/v1/meta"),
	"errURLCredentials": errURLCredentials(),
	"errPathUnreadable": errPathUnreadable("/c/spool", errors.New("denied")),
	"errLeadingFlag":    errLeadingFlag("--json"),
	"errUnreachable":    errUnreachable("http://x/api/v1/meta", errors.New("refused")),
	"errClientTooOld":   errClientTooOld("v1.0.0", "v2.0.0"),
	"errInitNeedsStdin": errInitNeedsStdin(),
	"errInitOnlyFlags":  errInitOnlyFlags(),
	"errInitNeedsURL":   errInitNeedsURL(),
	"errInitBadURL":     errInitBadURL("ftp://x", "the scheme is not http or https"),
	"errInitKeyRead":    errInitKeyRead(errors.New("eof")),
	"errInitKeyEmpty":   errInitKeyEmpty(),
	"errInitKeyInvalid": errInitKeyInvalid(),
	"errInitKeyTooLong": errInitKeyTooLong(),
	"errInitExists":     errInitExists("/c.toml"),
	"errInitWrite":      errInitWrite("/c.toml", errors.New("denied")),
	"errSchemaKind":     errSchemaKind("x", []string{"envelope", "friction"}),
	"errSchemaVersion":  errSchemaVersion("friction", "9", []string{"1"}),
	"errSkillForm":      errSkillForm("x", []string{"skill-md", "prompt"}),
	"errSkillServer":    errSkillServer("ftp://x", "the scheme is not http or https"),
	"errSkillVerb":      errSkillVerb("list"),
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
