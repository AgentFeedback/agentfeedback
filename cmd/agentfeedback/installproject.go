package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
)

// projectTarget is the repository install --project works on and the
// instruction file it writes.
type projectTarget struct {
	repo, file string
}

// findProject is the nearest ancestor of the working directory holding
// .git (a directory or a file), and its AGENTS.md, else its CLAUDE.md when
// only that exists, else a new AGENTS.md.
func findProject() (projectTarget, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return projectTarget{}, failErr("cannot read the working directory: "+oneLine(err.Error()), "run the command from inside the repository")
	}
	for d := cwd; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			t := projectTarget{repo: d, file: filepath.Join(d, "AGENTS.md")}
			if _, err := os.Lstat(t.file); errors.Is(err, fs.ErrNotExist) {
				if _, err := os.Lstat(filepath.Join(d, "CLAUDE.md")); err == nil {
					t.file = filepath.Join(d, "CLAUDE.md")
				}
			}

			return t, nil
		}
		if filepath.Dir(d) == d {
			return projectTarget{}, failErr("no git repository contains "+cwd, "run agentfeedback install --project from inside the repository")
		}
	}
}

// projectCheck is the project file as install --check --project reports
// it.
type projectCheck struct {
	Repo string `json:"repo"`
	File string `json:"file"`
	Rule string `json:"rule"`
}

func (t projectTarget) check() (projectCheck, error) {
	c := projectCheck{Repo: t.repo, File: t.file, Rule: harness.RuleMissing}
	want, err := harness.ProjectSection()
	if err != nil {
		return c, err
	}
	data, err := os.ReadFile(t.file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return c, nil
	case err != nil:
		return c, failErr(fmt.Sprintf("cannot read %s: %s", t.file, oneLine(err.Error())), "fix the file, then run the command again")
	}
	c.Rule = harness.SectionState(t.file, data, want)

	return c, nil
}

// ruleCheckOutcome is the JSON install --check prints.
type ruleCheckOutcome struct {
	Status    string              `json:"status"`
	Harnesses []harness.RuleCheck `json:"harnesses"`
	Project   *projectCheck       `json:"project,omitempty"`
}

// runInstallCheck reports the rule section of each harness (no names:
// the detected and the recorded ones) and, with project, the repository's
// pointer section. It writes nothing and takes no lock; any missing or
// stale section exits 3.
func runInstallCheck(env harness.Env, pos []string, project, asJSON bool, stdout io.Writer) error {
	var names []string
	if len(pos) == 0 || slices.Contains(pos, "all") {
		recorded, err := env.Recorded()
		if err != nil {
			return printInstallOutcome(stdout, installOutcome{}, installErr(err))
		}
		for _, n := range harness.Names() {
			if env.Detect(n) != "no" || slices.Contains(recorded, n) || slices.Contains(pos, n) {
				names = append(names, n)
			}
		}
	} else {
		names = dedupe(pos)
	}
	checks, err := env.Check(names)
	if err != nil {
		return printInstallOutcome(stdout, installOutcome{}, installErr(err))
	}
	out := ruleCheckOutcome{Status: "current", Harnesses: checks}
	if project {
		t, err := findProject()
		if err != nil {
			return printInstallOutcome(stdout, installOutcome{}, err)
		}
		pc, err := t.check()
		if err != nil {
			return printInstallOutcome(stdout, installOutcome{}, err)
		}
		out.Project = &pc
		if pc.Rule != harness.RuleCurrent {
			out.Status = "not_current"
		}
	}
	for _, c := range checks {
		if c.Rule == harness.RuleMissing || c.Rule == harness.RuleStale {
			out.Status = "not_current"
		}
	}
	if asJSON {
		if err := writeJSON(stdout, out); err != nil {
			return err
		}
	} else {
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "HARNESS\tFILE\tRULE")
		for _, c := range checks {
			file := c.File
			if file == "" {
				file = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Name, file, c.Rule)
		}
		if out.Project != nil {
			fmt.Fprintf(tw, "project\t%s\t%s\n", out.Project.File, out.Project.Rule)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if out.Status != "current" {
		return exitStatus(3)
	}

	return nil
}

// runInstallProject writes, or with uninstall removes, the pointer section
// in the repository's instruction file after confirmation. It never runs
// git, takes no backup and never reads or writes the install manifest.
func runInstallProject(uninstall, yes, dryRun bool, stdin io.Reader, stdout, stderr io.Writer) error {
	t, err := findProject()
	if err != nil {
		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	text, err := harness.ProjectSection()
	if err != nil {
		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	data, exists, err := readProjectFile(t.file)
	if err != nil {
		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	var out []byte
	var se *harness.SectionError
	if uninstall {
		var found bool
		out, found, err = harness.RemoveSection(t.file, data, "\n"+text)
		if err == nil && !found {
			out = data
		}
	} else {
		out, _, err = harness.SetSection(t.file, data, text)
	}
	if errors.As(err, &se) {
		return printInstallOutcome(stdout, installOutcome{}, failErr(se.Error(), "leave at most one agentfeedback section with both its markers, then run the command again"))
	}
	if err != nil {
		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	remove := uninstall && exists && len(out) == 0
	command := "install"
	status := "installed"
	if uninstall {
		status = "uninstalled"
	}
	if changed := !bytes.Equal(out, data) || (!exists && !uninstall); !changed {
		res := installOutcome{Status: "unchanged"}
		report(stderr, command, res, nil, false)

		return printInstallOutcome(stdout, res, nil)
	}
	res := installOutcome{Status: status, Changed: []string{t.file}}
	if dryRun {
		res.Status = "dry_run"
		report(stderr, command, res, nil, true)

		return printInstallOutcome(stdout, res, nil)
	}
	if !yes {
		if !isTerminal() {
			return printInstallOutcome(stdout, installOutcome{}, errProjectConfirm(t))
		}
		prompt := "Write the AgentFeedback pointer section into %s in %s? [y/N] "
		if uninstall {
			prompt = "Remove the AgentFeedback pointer section from %s in %s? [y/N] "
		}
		fmt.Fprintf(stderr, prompt, filepath.Base(t.file), t.repo)
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return printInstallOutcome(stdout, installOutcome{}, failErr("not confirmed; "+t.file+" was not changed", "run the command again and answer y"))
		}
	}
	now, nowExists, err := readProjectFile(t.file)
	if err != nil {
		return printInstallOutcome(stdout, installOutcome{}, err)
	}
	if nowExists != exists || !bytes.Equal(now, data) {
		return printInstallOutcome(stdout, installOutcome{}, failErr(t.file+" changed while install --project ran", "run the command again"))
	}
	if remove {
		err = os.Remove(t.file)
		if err == nil {
			err = syncProjectDir(filepath.Dir(t.file))
		}
	} else {
		err = writeProjectFile(t.file, out)
	}
	if err != nil {
		return printInstallOutcome(stdout, installOutcome{}, failErr(fmt.Sprintf("cannot update %s: %s", t.file, oneLine(err.Error())), "fix or restore that file, then run the command again"))
	}
	report(stderr, command, res, nil, false)

	return printInstallOutcome(stdout, res, nil)
}

func errProjectConfirm(t projectTarget) error {
	return failErr("install --project changes "+t.file+" in "+t.repo+" only after confirmation, and stdin is not a terminal", "rerun with --yes to confirm")
}

// readProjectFile is the instruction file's bytes and whether it exists.
// A symbolic link or another non-regular file is refused: install
// --project edits only regular files.
func readProjectFile(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, false, failErr(fmt.Sprintf("cannot read %s: %s", path, oneLine(err.Error())), "fix the file, then run the command again")
	case info.Mode()&fs.ModeSymlink != 0:
		return nil, false, failErr(path+" is a symbolic link; agentfeedback install --project edits only regular files", "replace the link with the file it points to, then run the command again")
	case !info.Mode().IsRegular():
		return nil, false, failErr(path+" is not a regular file; agentfeedback install --project edits only regular files", "replace it with a regular file, then run the command again")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, failErr(fmt.Sprintf("cannot read %s: %s", path, oneLine(err.Error())), "fix the file, then run the command again")
	}

	return data, true, nil
}

// writeProjectFile replaces path with data through a temporary file in the
// same directory, keeping the mode of an existing file, and makes the
// rename durable.
func writeProjectFile(path string, data []byte) error {
	mode := fs.FileMode(0o644)
	if info, err := os.Lstat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}

	if err := os.Rename(name, path); err != nil {
		return err
	}

	return syncProjectDir(filepath.Dir(path))
}

// syncProjectDir makes a rename or removal in dir durable; a variable so
// tests can see it called.
var syncProjectDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}

	return err
}
