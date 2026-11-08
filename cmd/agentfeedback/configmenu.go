package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
)

// runConfigMenu shares one reader with install so an answer already buffered
// after a menu selection remains available to install's destination prompt.
func runConfigMenu(stdin io.Reader, stdout, stderr io.Writer) error {
	in := bufio.NewReader(stdin)
	keys := configEditableKeys()
	for {
		view, err := loadConfigView(modeFlags{}, os.Getenv)
		if err != nil {
			return err
		}
		if err := printConfigView(stdout, view); err != nil {
			return err
		}
		if err := printConfigMenu(stdout, keys); err != nil {
			return err
		}
		if _, err := fmt.Fprint(stderr, "Choice: "); err != nil {
			return err
		}
		answer, ok, err := readConfigMenuLine(in)
		if err != nil || !ok {
			return err
		}
		choice, err := strconv.Atoi(answer)
		if err != nil || choice < 0 || choice > len(keys)+3 {
			if _, err := fmt.Fprintln(stderr, "Choose one of the numbered options."); err != nil {
				return err
			}
			continue
		}
		if choice == 0 {
			return nil
		}
		if choice <= len(keys) {
			key := keys[choice-1]
			if _, err := fmt.Fprintf(stderr, "New value for %s (lists: comma-separated or JSON; empty clears a list): ", key); err != nil {
				return err
			}
			raw, ok, err := readConfigMenuLine(in)
			if err != nil || !ok {
				return err
			}
			if err := setConfigSetting(os.Getenv, key, raw); err != nil {
				if _, err := fmt.Fprintf(stderr, "Cannot update %s: %v\n", key, err); err != nil {
					return err
				}
			} else if _, err := fmt.Fprintf(stdout, "Updated %s.\n", key); err != nil {
				return err
			}
			continue
		}
		if err := runInstall([]string{"--list"}, in, stdout, stderr); err != nil {
			if _, err := fmt.Fprintf(stderr, "Cannot list harnesses: %v\n", err); err != nil {
				return err
			}
			continue
		}
		if choice == len(keys)+1 {
			continue
		}
		action := "install"
		if choice == len(keys)+3 {
			action = "uninstall"
		}
		if _, err := fmt.Fprintf(stderr, "One harness to %s (name; empty cancels): ", action); err != nil {
			return err
		}
		name, ok, err := readConfigMenuLine(in)
		if err != nil || !ok {
			return err
		}
		if name == "" {
			continue
		}
		// Do not pass flags, multiple targets, or 'all' to the wiring commands.
		if !harness.Known(name) {
			if _, err := fmt.Fprintln(stderr, "Choose exactly one known harness name from the listing; all is not allowed."); err != nil {
				return err
			}
			continue
		}
		if action == "install" {
			err = runInstall([]string{name}, in, stdout, stderr)
		} else {
			err = runUninstall([]string{name}, in, stdout, stderr)
		}
		if err != nil {
			if _, err := fmt.Fprintf(stderr, "Cannot %s harness: %v\n", action, err); err != nil {
				return err
			}
		}
	}
}

func printConfigMenu(stdout io.Writer, keys []string) error {
	if _, err := fmt.Fprintln(stdout, "\nMachine configuration menu (destination, organization policy and repository constraints are read-only):"); err != nil {
		return err
	}
	for i, key := range keys {
		if _, err := fmt.Fprintf(stdout, "  %d. Edit %s\n", i+1, key); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(stdout, "  %d. List harnesses\n  %d. Install one harness\n  %d. Uninstall one harness\n  0. Quit\n", len(keys)+1, len(keys)+2, len(keys)+3)

	return err
}

// A final unterminated answer is usable, but an empty EOF is not an empty
// answer: in particular, it must never clear a list or choose a harness.
func readConfigMenuLine(in *bufio.Reader) (string, bool, error) {
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, err
	}

	return strings.TrimSpace(line), len(line) != 0, nil
}
