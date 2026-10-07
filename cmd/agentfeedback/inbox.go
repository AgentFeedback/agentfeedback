package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
	"github.com/agentfeedback/agentfeedback/v4/pkg/envelope"
	"github.com/agentfeedback/agentfeedback/v4/pkg/schema"
)

const (
	ingestSynopsis = "ingest [--json] [--local | --server URL]"
	inboxSuffix    = ".json"
	inboxDone      = "done"
	inboxRejected  = "rejected"
	reasonSuffix   = ".reason"
	// originInbox is the context.origin every inbox row carries.
	originInbox = "inbox"
	// inboxSettle is how long a file that is not one JSON object is left
	// alone before it is refused: a writer that skipped the temporary file
	// may still be writing it.
	inboxSettle = time.Minute
)

// inboxReport is what one pass over the inbox did, as ingest prints it.
// Pending files stay in the inbox for the next pass; skipped ones (symlinks
// and other non-regular entries) stay too and are named every pass.
type inboxReport struct {
	Ingested   int `json:"ingested"`
	Duplicates int `json:"duplicates"`
	Rejected   int `json:"rejected"`
	Pending    int `json:"pending"`
	Skipped    int `json:"skipped"`
}

// ingestInbox delivers the inbox, ${data}/inbox/*.json, one envelope per
// file, through c: the create path of every other submission. It reads the
// directory once and never watches it. Dot-prefixed names and names not
// ending in .json are not candidates (a writer's temporary file). Per file:
//
//	accepted (created or duplicate)          moved to done/<id>-<name>
//	refused for good, over the create cap,   moved to rejected/<name> with
//	not one JSON object, key mismatch        <name>.reason beside it
//	not one JSON object, written in the      left for the next pass (a
//	last minute                              writer may still be writing)
//	destination busy or unreachable,         left; the pass stops, so a
//	key or URL wrong                         busy database costs one wait
//	symlink or not a regular file            left, never read, named
//
// Every file operation goes through an os.Root on the inbox, so nothing
// resolves outside it even if a directory is swapped for a link mid-pass;
// entries are checked with Lstat, and the open file must be the one checked
// (the root follows a link that stays inside it, so an entry swapped for one
// after the check is caught here and never read). A file over the create
// cap is refused from its size, unread. A file is moved only while its name
// still holds the file that was read, by identity, size and modification
// time: one replaced or rewritten under the same name meanwhile stays for
// the next pass. Each body gets context.origin "inbox" and, when it has
// no key, a key derived from the file name and bytes, so two passes racing
// over one file, or a pass that stopped between the create and the move,
// end in a duplicate, never a second row. note receives one line per file
// named; every outcome and every file that is left is also logged to the
// client log. The pass stops early, leaving the rest, when ctx is done.
func ingestInbox(ctx context.Context, c *client.Client, data string, note func(string)) (inboxReport, error) {
	var rep inboxReport
	dir := client.InboxDir(data)
	root, names, err := openInbox(dir)
	if err != nil || root == nil {
		return rep, err
	}
	defer func() { _ = root.Close() }()
	logLeft := func(msg string) {
		note(msg)
		c.Log(client.Outcome{Outcome: client.OutcomeError, Reason: "inbox: " + msg})
	}
	skip := func(name, why string) {
		rep.Skipped++
		logLeft(fmt.Sprintf("skipped %s: %s; it is not read", filepath.Join(dir, name), why))
	}
	reject := func(name string, info fs.FileInfo, o client.Outcome, problem []byte) {
		c.Log(o)
		dest, err := rejectInboxFile(root, info, name, o, problem)
		if errors.Is(err, errInboxGone) {
			return // another pass took it
		}
		if err != nil {
			rep.Pending++
			logLeft(fmt.Sprintf("refused %s (%s) but cannot move it to %s: %v; it is left", name, o.Reason, inboxRejected, err))

			return
		}
		rep.Rejected++
		path := filepath.Join(dir, dest)
		note(fmt.Sprintf("rejected %s: %s; moved to %s, the reason is in %s", name, inboxWhy(o), path, path+reasonSuffix))
	}
	for i, name := range names {
		if ctx.Err() != nil {
			rep.Pending += len(names) - i
			logLeft(fmt.Sprintf("the pass ran out of time; %d file(s) left in %s", len(names)-i, dir))

			break
		}
		body, info, why, err := readInboxFile(root, name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue // another pass took it
		case errors.Is(err, errInboxTooLarge):
			reject(name, info, client.Outcome{
				Outcome: client.OutcomeRejected, Reason: "body_too_large",
				Message: fmt.Sprintf("the file is over the %d-byte create limit; it was not read", envelope.BodyLimit),
			}, nil)

			continue
		case err != nil:
			rep.Pending++
			logLeft(fmt.Sprintf("cannot read %s: %v; it is left for the next pass", filepath.Join(dir, name), err))

			continue
		case why != "":
			skip(name, why)

			continue
		}
		prepared, err := inboxBody(name, body)
		if age := nowFunc().Sub(info.ModTime()); err != nil && age >= 0 && age < inboxSettle {
			rep.Pending++
			logLeft(fmt.Sprintf("%s is not one JSON object yet and was written in the last minute; it is left for the next pass", name))

			continue
		}
		if err != nil {
			reject(name, info, client.Outcome{Outcome: client.OutcomeRejected, Reason: "invalid_body", Message: err.Error()}, nil)

			continue
		}
		if len(prepared) > envelope.BodyLimit {
			reject(name, info, client.Outcome{
				Outcome: client.OutcomeRejected, Reason: "body_too_large",
				Message: fmt.Sprintf("with the key and origin added the body is %d bytes, over the %d-byte create limit; it was not sent", len(prepared), envelope.BodyLimit),
			}, nil)

			continue
		}
		d := c.Deliver(ctx, prepared)
		switch {
		case d.Hold || d.Retry:
			c.Log(d.Outcome)
			rep.Pending += len(names) - i
			what := "the destination did not take it now"
			if d.Hold {
				what = "the destination refused the setup; run agentfeedback doctor"
			}
			note(fmt.Sprintf("%s (%s); %d file(s) left in %s for the next pass", what, d.Outcome.Reason, len(names)-i, dir))

			return rep, nil
		case d.Outcome.Outcome == client.OutcomeSubmitted || d.Outcome.Outcome == client.OutcomeDuplicate:
			if d.Outcome.Outcome == client.OutcomeDuplicate {
				rep.Duplicates++
			} else {
				rep.Ingested++
			}
			c.Log(d.Outcome)
			id := strconv.FormatInt(d.Outcome.ID, 10)
			err := moveInboxFile(root, info, name, filepath.Join(inboxDone, id+"-"+fitName(name, nameMax-len(id)-1)))
			if err != nil && !errors.Is(err, errInboxGone) {
				logLeft(fmt.Sprintf("stored %s as submission %d but cannot move it to %s: %v; the next pass finds the duplicate and moves it", name, d.Outcome.ID, inboxDone, err))
			}
		default:
			reject(name, info, d.Outcome, d.Problem)
		}
	}

	return rep, nil
}

// inboxPass is ingestInbox as the start-up pass and flush run it: nothing
// printed, a failure to read the inbox logged as one error line.
func inboxPass(ctx context.Context, c *client.Client, data string) {
	if _, err := ingestInbox(ctx, c, data, func(string) {}); err != nil {
		c.Log(client.Outcome{Outcome: client.OutcomeError, Reason: "inbox: " + oneLine(err.Error())})
	}
}

// openInbox opens the inbox as a root and lists the names a pass
// considers: not dot-prefixed, ending in .json, in directory order. A
// missing inbox returns a nil root. The inbox itself must be a directory,
// not a link to one: the opened root must be the directory Lstat saw.
func openInbox(dir string) (*os.Root, []string, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, errInboxNotDir(dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	names, err := inboxNames(root, info, dir)
	if err != nil {
		_ = root.Close()

		return nil, nil, err
	}

	return root, names, nil
}

// inboxNames lists the candidates of an opened inbox root after checking it
// is the directory info describes.
func inboxNames(root *os.Root, info fs.FileInfo, dir string) ([]string, error) {
	st, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, st) {
		return nil, errInboxNotDir(dir)
	}
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if n := e.Name(); !strings.HasPrefix(n, ".") && strings.HasSuffix(n, inboxSuffix) {
			names = append(names, n)
		}
	}

	return names, nil
}

// inboxHasEntries reports whether the inbox under data holds any name a
// pass considers, a symlink included; any error counts as yes.
func inboxHasEntries(data string) bool {
	root, names, err := openInbox(client.InboxDir(data))
	if root != nil {
		_ = root.Close()
	}

	return err != nil || len(names) > 0
}

// inboxHasCandidates reports whether the inbox under data holds a regular
// file a pass would read; any error counts as yes, so the pass reports it.
// A symlink or other entry alone does not count: no pass would deliver it.
func inboxHasCandidates(data string) bool {
	root, names, err := openInbox(client.InboxDir(data))
	if root == nil || err != nil {
		return err != nil
	}
	defer func() { _ = root.Close() }()
	for _, n := range names {
		if info, err := root.Lstat(n); err == nil && info.Mode().IsRegular() {
			return true
		}
	}

	return false
}

// errInboxTooLarge marks a file over the create cap; it is not read past
// the cap.
var errInboxTooLarge = errors.New("over the create limit")

// readInboxFile reads one inbox file, never through a link, and returns the
// Lstat of what it read. why is set, and nothing read, when the entry is
// not a regular file or changed between the check and the open. A file over
// the create cap, by its size or by what a read finds, is errInboxTooLarge.
func readInboxFile(root *os.Root, name string) (body []byte, info fs.FileInfo, why string, err error) {
	info, err = root.Lstat(name)
	if err != nil {
		return nil, nil, "", err
	}
	if w := notRegular(info.Mode()); w != "" {
		return nil, info, w, nil
	}
	if info.Size() > envelope.BodyLimit {
		return nil, info, "", errInboxTooLarge
	}
	f, err := root.OpenFile(name, os.O_RDONLY|inboxOpenFlags, 0)
	if err != nil {
		return nil, info, "", err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, info, "", err
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return nil, info, "it changed while it was opened", nil
	}
	if opened.Size() > envelope.BodyLimit {
		return nil, info, "", errInboxTooLarge
	}
	body, err = io.ReadAll(io.LimitReader(f, envelope.BodyLimit+1))
	if err == nil && len(body) > envelope.BodyLimit {
		return nil, info, "", errInboxTooLarge
	}

	return body, info, "", err
}

// notRegular says why an entry of mode m is not read, or "" for a regular
// file.
func notRegular(m fs.FileMode) string {
	switch {
	case m.IsRegular():
		return ""
	case m&fs.ModeSymlink != 0:
		return "it is a symlink"
	case m.IsDir():
		return "it is a directory"
	}

	return "it is not a regular file"
}

// inboxBody is the file's object with context.origin set to "inbox" and,
// when key is absent, null or blank, the key inboxKey derives. Every other
// byte of every member is kept. A context that is present but not an
// object moves to the top-level member context_raw, which the create path
// keeps in the payload, as it does with such a context itself. A leading
// UTF-8 byte order mark, which some Windows writers add, is dropped.
func inboxBody(name string, body []byte) ([]byte, error) {
	obj, err := parseObject(bytes.TrimPrefix(body, []byte("\xef\xbb\xbf")))
	if err != nil {
		return nil, fmt.Errorf("the file is not one JSON object (%v); write one envelope per file", err)
	}
	raw, ok := obj.get("context")
	switch {
	case !ok || string(raw) == "null":
		obj = obj.set("context", rawObject{}.setString("origin", originInbox).encode())
	case !isObject(raw):
		if obj.has("context_raw") {
			break // the writer's own context_raw is kept; the server moves this context itself
		}
		obj = obj.set("context_raw", raw).set("context", rawObject{}.setString("origin", originInbox).encode())
	default:
		ctx, err := parseObject(raw)
		if err != nil {
			return nil, fmt.Errorf("context is not one JSON object (%v)", err)
		}
		obj = obj.set("context", ctx.setString("origin", originInbox).encode())
	}
	if raw, ok := obj.get("key"); !ok || keyBlank(raw) {
		obj = obj.setString("key", inboxKey(name, body))
	}

	return obj.encode(), nil
}

// keyBlank reports whether a key value is null or a string blank after
// trimming; any other type is left for the create path to refuse.
func keyBlank(raw json.RawMessage) bool {
	if string(raw) == "null" {
		return true
	}
	var s string

	return json.Unmarshal(raw, &s) == nil && schema.Trim(s) == ""
}

// inboxKey is the key of an inbox file that names none: fixed by its name
// and bytes, so the same file delivered twice is one row.
func inboxKey(name string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(body)

	return "inbox-" + hex.EncodeToString(h.Sum(nil))[:32]
}

// inboxSubdir makes sure sub exists in root, created 0700 when missing, and
// is a directory itself, never a link.
func inboxSubdir(root *os.Root, sub string) error {
	if err := root.Mkdir(sub, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	st, err := root.Lstat(sub)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errInboxNotDir(filepath.Join(root.Name(), sub))
	}

	return nil
}

// errInboxChanged: the name no longer holds the file that was read.
var errInboxChanged = errors.New("the name now holds another file, which is left for the next pass")

// errInboxGone: the file is no longer in the inbox; another pass moved it.
var errInboxGone = errors.New("the file is gone from the inbox")

// sameInboxFile reports whether b is still the file a describes: the same
// file, size and modification time. A file deleted and written again can
// get the same inode back, and a rewrite in place keeps it.
func sameInboxFile(a, b fs.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// moveInboxFile renames name to dest (a path under one of the inbox's
// subdirectories) while name still holds the file info describes. A file
// already gone is errInboxGone.
func moveInboxFile(root *os.Root, info fs.FileInfo, name, dest string) error {
	if err := inboxSubdir(root, filepath.Dir(dest)); err != nil {
		return err
	}
	st, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return errInboxGone
	}
	if err != nil {
		return err
	}
	if !sameInboxFile(info, st) {
		return errInboxChanged
	}
	if err := root.Rename(name, dest); errors.Is(err, fs.ErrNotExist) {
		return errInboxGone
	} else if err != nil {
		return err
	}

	return nil
}

// nameMax is the longest file name the common file systems take, in bytes.
const nameMax = 255

// fitName keeps at most the last max bytes of name, starting on a whole
// UTF-8 character, so a prefix or a suffix added to it still fits a file
// name; the .json ending survives.
func fitName(name string, max int) string {
	if len(name) <= max {
		return name
	}
	start := len(name) - max
	for start < len(name) && !utf8.RuneStart(name[start]) {
		start++
	}

	return name[start:]
}

// rejectInboxFile writes the reason sidecar and moves the file into
// rejected/, keeping its name unless an earlier file or sidecar holds it,
// then adding the time. It returns the file's new path relative to the
// inbox. The sidecar is created exclusively, so an existing entry of that
// name, a link included, is never written through; it holds the outcome
// line, then the destination's error body when there is one.
func rejectInboxFile(root *os.Root, info fs.FileInfo, name string, o client.Outcome, problem []byte) (string, error) {
	if err := inboxSubdir(root, inboxRejected); err != nil {
		return "", err
	}
	var reason strings.Builder
	if err := o.Write(&reason); err != nil {
		return "", err
	}
	if len(problem) > 0 {
		reason.Write(problem)
		reason.WriteByte('\n')
	}
	stem := strings.TrimSuffix(name, inboxSuffix)
	for attempt := 0; ; attempt++ {
		dest := fitName(name, nameMax-len(reasonSuffix))
		if attempt > 0 {
			dest = fitName(fmt.Sprintf("%s-%s-%d%s", stem, nowFunc().UTC().Format("20060102T150405Z"), attempt, inboxSuffix), nameMax-len(reasonSuffix))
		}
		dest = filepath.Join(inboxRejected, dest)
		if _, err := root.Lstat(dest); err == nil {
			continue
		}
		f, err := root.OpenFile(dest+reasonSuffix, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) && attempt < 10 {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.WriteString(reason.String())
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr == nil {
			werr = moveInboxFile(root, info, name, dest)
		}
		if werr != nil {
			_ = root.Remove(dest + reasonSuffix)

			return "", werr
		}

		return dest, nil
	}
}

// inboxWhy is the one-line reason of a refused file.
func inboxWhy(o client.Outcome) string {
	if o.Message != "" {
		return o.Reason + ": " + oneLine(o.Message)
	}

	return o.Reason
}

// runIngest is the ingest command: one pass over the inbox, then the counts.
func runIngest(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	skipStartupPass = true
	flags := newFlagSet("ingest")
	mf := addModeFlags(flags)
	asJSON := flags.Bool("json", false, "print the counts as one JSON object")
	pos, err := parseInterleaved(flags, args, stderr)
	if err != nil {
		return errFlags("ingest", err)
	}
	if len(pos) != 0 {
		return errArgs("ingest", ingestSynopsis)
	}
	s, err := newSubmitter("ingest", false, *mf, stdout, stderr)
	if err != nil {
		return err
	}
	if o, off := s.disabled(); off {
		return finish("ingest", o, stdout)
	}
	data, err := dataDir(os.Getenv)
	if err != nil {
		return err
	}
	var rep inboxReport
	m, err := resolveMode(*mf, os.Getenv)
	if err != nil {
		return err
	}
	// An empty inbox in local mode needs no database: do not create one.
	// A symlink alone still runs the pass, which names it.
	if m.Mode != modeLocal || inboxHasEntries(data) {
		c, err := s.client()
		if err != nil {
			return err
		}
		rep, err = ingestInbox(context.Background(), c, data, func(msg string) {
			fmt.Fprintf(stderr, "agentfeedback ingest: %s\n", msg)
		})
		if err != nil {
			return err
		}
	}
	if *asJSON {
		return writeJSON(stdout, rep)
	}
	_, err = fmt.Fprintf(stdout, "ingested %d, duplicates %d, rejected %d, pending %d, skipped %d\n",
		rep.Ingested, rep.Duplicates, rep.Rejected, rep.Pending, rep.Skipped)

	return err
}

func errInboxNotDir(path string) error {
	return failErr(fmt.Sprintf("%s is not a directory (a symlink or a file); the inbox is not read", path),
		"remove it; agentfeedback creates the inbox's done/ and rejected/ itself, and a writer creates inbox/ with mode 0700")
}
