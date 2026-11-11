package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"

	checkup "github.com/agentfeedback/agentfeedback/v4/internal/report"
)

const (
	reportSynopsis       = "report check|render <report.json>"
	reportCheckSynopsis  = "report check <report.json>|- [--json]"
	reportRenderSynopsis = "report render <report.json>|- [--out FILE] [--open]"
)

// reportSubcommands are the subcommands of report, in help order.
var reportSubcommands = []string{"check", "render"}

// openFile shows a written page in the default browser; tests replace it so
// no browser starts.
var openFile = startOpener

func runReport(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errArgs("report", reportSynopsis)
	}
	switch args[0] {
	case "-h", "--help", "-help":
		fmt.Fprintf(stderr, "usage: agentfeedback %s\n  %s\n  %s\n", reportSynopsis, reportCheckSynopsis, reportRenderSynopsis)

		return flag.ErrHelp
	case "check":
		return runReportCheck(args[1:], stdin, stdout, stderr)
	case "render":
		return runReportRender(args[1:], stdin, stdout, stderr)
	}

	return usageErr(fmt.Sprintf("unknown report subcommand %q", args[0]), "use agentfeedback "+reportSynopsis)
}

// checkOutput is report check --json: scores is null when the report is
// invalid, problems empty when it is valid.
type checkOutput struct {
	Valid    bool              `json:"valid"`
	Problems []checkup.Problem `json:"problems"`
	Scores   *checkup.Scores   `json:"scores"`
}

func runReportCheck(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("report check")
	asJSON := fs.Bool("json", false, "print {valid, problems, scores} as JSON on stdout")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s: validate a checkup report and print the grade the binary computes; - reads stdin; an invalid report exits 2\n", reportCheckSynopsis)
		fs.PrintDefaults()
	}
	pos, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("report check", err)
	}
	if len(pos) != 1 {
		return errArgs("report check", reportCheckSynopsis)
	}
	r, probs, err := readReport(pos[0], stdin)
	if err != nil {
		return err
	}
	if *asJSON {
		out := checkOutput{Valid: len(probs) == 0, Problems: probs}
		if out.Valid {
			s := checkup.Grade(r)
			out.Scores = &s
		}
		if out.Problems == nil {
			out.Problems = []checkup.Problem{}
		}
		if err := writeJSON(stdout, out); err != nil {
			return err
		}
		if !out.Valid {
			return exitStatus(2)
		}

		return nil
	}
	if len(probs) > 0 {
		printProblems(stderr, probs)

		return exitStatus(2)
	}

	return printScores(stdout, checkup.Grade(r))
}

func runReportRender(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("report render")
	out := fs.String("out", "", "write the page to this file, replacing an existing one (not the report itself); default <cache dir>/reports/checkup-<UTC time>.html, never replaced: -2, -3, ... is added on a collision")
	open := fs.Bool("open", false, "open the page in the default browser after writing it")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s: check a checkup report, then write it as one self-contained HTML page and print its path; - reads stdin; an invalid report exits 2 and writes nothing\n", reportRenderSynopsis)
		fs.PrintDefaults()
	}
	pos, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("report render", err)
	}
	if len(pos) != 1 {
		return errArgs("report render", reportRenderSynopsis)
	}
	if *out != "" && pos[0] != "-" && sameFile(*out, pos[0]) {
		return usageErr(fmt.Sprintf("--out %s is the report being rendered", *out), "pass another --out path, or none for the cache directory")
	}
	r, probs, err := readReport(pos[0], stdin)
	if err != nil {
		return err
	}
	if len(probs) > 0 {
		printProblems(stderr, probs)

		return exitStatus(2)
	}
	var page bytes.Buffer
	if err := checkup.Render(&page, r); err != nil {
		return failErr("the page cannot be rendered: "+oneLine(err.Error()), "report this as a bug")
	}
	path, replace := *out, true
	if path == "" {
		replace = false
		cache, err := cacheDir(os.Getenv)
		if err != nil {
			return err
		}
		dir := filepath.Join(cache, "reports")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return failErr(fmt.Sprintf("cannot create %s: %s", dir, oneLine(err.Error())), "pass --out FILE")
		}
		path = filepath.Join(dir, "checkup-"+nowFunc().UTC().Format("20060102T150405Z")+".html")
	}
	path, err = writePrivateFile(path, page.Bytes(), replace)
	if err != nil {
		return failErr(fmt.Sprintf("cannot write %s: %s", path, oneLine(err.Error())), "pass --out with a writable path")
	}
	if _, err := fmt.Fprintln(stdout, path); err != nil {
		return err
	}
	if *open {
		// An absolute path never starts with - and never reads as a URI
		// scheme to the opener.
		abs, err := filepath.Abs(path)
		if err == nil {
			err = openFile(abs)
		}
		if err != nil {
			fmt.Fprintf(stderr, "agentfeedback report render: warning: cannot open the page: %s; open %s yourself\n", oneLine(err.Error()), path)
		}
	}

	return nil
}

// readReport reads the report at name, or stdin for -, and checks it. A file
// that cannot be read is an error; a report that is not valid comes back as
// its problems.
func readReport(name string, stdin io.Reader) (checkup.Report, []checkup.Problem, error) {
	var raw []byte
	var err error
	if name == "-" {
		raw, err = io.ReadAll(stdin)
	} else {
		raw, err = os.ReadFile(name)
	}
	if err != nil {
		return checkup.Report{}, nil, failErr(fmt.Sprintf("cannot read %s: %s", name, oneLine(err.Error())), "pass the path of the report JSON, or - for stdin")
	}
	r, probs, err := checkup.Check(raw)
	if err != nil {
		return checkup.Report{}, nil, failErr("the report schema cannot be compiled: "+oneLine(err.Error()), "report this as a bug")
	}

	return r, probs, nil
}

func printProblems(w io.Writer, probs []checkup.Problem) {
	for _, p := range probs {
		fmt.Fprintf(w, "%s: %s\n", p.Pointer, p.Message)
	}
}

func printScores(w io.Writer, s checkup.Scores) error {
	score := "n/a"
	if s.Score != nil {
		score = strconv.FormatFloat(*s.Score, 'f', 4, 64)
	}
	fmt.Fprintf(w, "grade %s (score %s)\n", s.Grade, score)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUBRIC\tSCORE\tWEIGHT\tJUDGED\tINSUFFICIENT")
	for _, r := range s.Rubrics {
		rs := "n/a"
		if r.Score != nil {
			rs = strconv.FormatFloat(*r.Score, 'f', 4, 64)
		}
		fmt.Fprintf(tw, "%s\t%s\t%.4f\t%d\t%d\n", r.Name, rs, r.Weight, r.Judged, r.Insufficient)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "coverage %d of %d session(s) used a skill (not graded)\n", s.Coverage.WithSkills, s.Coverage.Sessions)
	if len(s.UsedSkills) > 0 {
		used := make([]string, len(s.UsedSkills))
		for i, u := range s.UsedSkills {
			used[i] = fmt.Sprintf("%s (%d)", u.Name, u.Sessions)
		}
		fmt.Fprintf(w, "used skills: %s\n", strings.Join(used, ", "))
	}
	if len(s.UnusedSkills) > 0 {
		fmt.Fprintf(w, "unused skills: %s\n", strings.Join(s.UnusedSkills, ", "))
	}

	return nil
}

// sameFile reports whether a and b name one existing file.
func sameFile(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)

	return err == nil && os.SameFile(sa, sb)
}

// writePrivateFile writes data, 0600, through a synced temporary file in the
// same directory, so a reader never sees half a page, and returns the path
// written. With replace the file is renamed over path. Without it an
// existing file is never touched: the file is linked into place, which fails
// on an existing name, and the next free name path-2, path-3, ... (before
// the extension) is tried.
func writePrivateFile(path string, data []byte, replace bool) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return path, err
	}
	tmpName := tmp.Name()
	// After a link the temporary name is a second name for the file, after a
	// rename it no longer exists: either way it goes.
	defer func() { _ = os.Remove(tmpName) }()
	_, werr := tmp.Write(data)
	if err := errors.Join(werr, tmp.Chmod(0o600), tmp.Sync()); err != nil {
		_ = tmp.Close()

		return path, err
	}
	if err := tmp.Close(); err != nil {
		return path, err
	}
	if replace {
		return path, os.Rename(tmpName, path)
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for n := 1; ; n++ {
		p := path
		if n > 1 {
			p = fmt.Sprintf("%s-%d%s", stem, n, ext)
		}
		err := os.Link(tmpName, p)
		if err == nil || !errors.Is(err, os.ErrExist) || n >= 1000 {
			return p, err
		}
	}
}

// startOpener starts the platform's opener on path and does not wait for it.
func startOpener(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()

	return nil
}
