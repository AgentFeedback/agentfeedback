package main

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	serverSynopsis = "server install --systemd|--launchd|--compose [--dir D] [--image REF] [--force]"
	launchdLabel   = "dev.agentfeedback.serve"
	imageRepo      = "ghcr.io/agentfeedback/agentfeedback"
	backupStamp    = "$(date -u +%Y%m%dT%H%M%SZ)"
)

// geteuid and executable are variables so tests can stand in for root and
// for the installed binary.
var (
	geteuid    = os.Geteuid
	executable = os.Executable
)

// serveEnv is what serve.env holds.
type serveEnv struct {
	keyFile, database, listen string
	port                      string
}

// serverOutcome is the one JSON line server install always ends with.
type serverOutcome struct {
	Status  string   `json:"status"`
	Mode    string   `json:"mode,omitempty"`
	Path    string   `json:"path,omitempty"`
	Next    []string `json:"next,omitempty"`
	Message string   `json:"message,omitempty"`
}

func printServerOutcome(stdout io.Writer, out serverOutcome, err error) error {
	if err == nil {
		out.Status = "written"

		return writeJSON(stdout, out)
	}
	if werr := writeJSON(stdout, serverOutcome{Status: "error", Message: err.Error()}); werr != nil {
		return werr
	}

	return &reportedError{err}
}

// runServer routes server subcommands; install is the only one.
func runServer(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errArgs("server", serverSynopsis)
	}
	if args[0] != "install" {
		if args[0] == "-h" || args[0] == "--help" || args[0] == "-help" {
			fmt.Fprintf(stderr, "usage: agentfeedback %s\n", serverSynopsis)

			return flag.ErrHelp
		}

		return errServerVerb(args[0])
	}
	fs := newFlagSet("server install")
	systemd := fs.Bool("systemd", false, "write a systemd user unit to ${XDG_CONFIG_HOME:-~/.config}/systemd/user/agentfeedback.service")
	launchd := fs.Bool("launchd", false, "write a launchd agent to ~/Library/LaunchAgents/"+launchdLabel+".plist")
	compose := fs.Bool("compose", false, "write <dir>/compose.yaml running the image")
	dir := fs.String("dir", "", "directory serve --init wrote (default ${XDG_CONFIG_HOME:-~/.config}/agentfeedback/server)")
	image := fs.String("image", "", "with --compose: image reference (default "+imageRepo+":<this release>)")
	force := fs.Bool("force", false, "replace an existing file")
	if err := parseFlags(fs, args[1:], stderr); err != nil {
		err = errFlags("server", err)
		if errors.Is(err, flag.ErrHelp) {
			return err
		}

		return printServerOutcome(stdout, serverOutcome{}, err)
	}
	if fs.NArg() != 0 {
		return printServerOutcome(stdout, serverOutcome{}, errArgs("server", serverSynopsis))
	}
	mode := ""
	n := 0
	for _, m := range []struct {
		on   bool
		name string
	}{{*systemd, "systemd"}, {*launchd, "launchd"}, {*compose, "compose"}} {
		if m.on {
			mode = m.name
			n++
		}
	}
	if n != 1 {
		return printServerOutcome(stdout, serverOutcome{}, errServerMode())
	}
	if visited(fs)["image"] && mode != "compose" {
		return printServerOutcome(stdout, serverOutcome{}, errServerImageFlag())
	}
	out, err := runServerInstall(os.Getenv, mode, *dir, *image, *force, stderr)

	return printServerOutcome(stdout, out, err)
}

// runServerInstall writes the one file of mode from serve.env. It starts,
// enables and reloads nothing; the next commands are printed instead.
func runServerInstall(getenv func(string) string, mode, dir, image string, force bool, stderr io.Writer) (serverOutcome, error) {
	if err := checkSupported("server install"); err != nil {
		return serverOutcome{}, err
	}
	if mode == "systemd" && geteuid() == 0 {
		return serverOutcome{}, errServerRoot()
	}
	var err error
	if dir == "" {
		if dir, err = serverDir(getenv); err != nil {
			return serverOutcome{}, err
		}
	}
	if dir, err = absSafe("the server directory", dir); err != nil {
		return serverOutcome{}, err
	}
	envPath := filepath.Join(dir, serveEnvName)
	env, err := readServeEnv(envPath)
	if err != nil {
		return serverOutcome{}, err
	}
	// The key file is checked, never read.
	if info, err := os.Stat(env.keyFile); errors.Is(err, os.ErrNotExist) {
		return serverOutcome{}, errServerKeyFile(env.keyFile, "does not exist")
	} else if err != nil {
		return serverOutcome{}, errServerKeyFile(env.keyFile, "cannot be checked: "+pathCause(err))
	} else if !info.Mode().IsRegular() {
		return serverOutcome{}, errServerKeyFile(env.keyFile, "is not a regular file")
	}
	var bin, backup string
	if mode != "compose" {
		if bin, err = binaryPath(); err != nil {
			return serverOutcome{}, err
		}
		backup = fmt.Sprintf("set -a; . %s; set +a; %s backup %s",
			envPath, bin, filepath.Join(filepath.Dir(env.database), "agentfeedback-"+backupStamp+".db"))
	}

	var path, logDir string
	var data []byte
	var next []string
	var notes []string
	parentMode := os.FileMode(0o755)
	switch mode {
	case "systemd":
		cfg, err := xdgBase(getenv, "XDG_CONFIG_HOME", ".config")
		if err != nil {
			return serverOutcome{}, err
		}
		path = filepath.Join(cfg, "systemd", "user", "agentfeedback.service")
		data = systemdUnit(envPath, bin)
		next = []string{
			"systemctl --user daemon-reload",
			"systemctl --user enable --now agentfeedback.service",
			"systemctl --user status agentfeedback.service",
			`loginctl enable-linger "$USER"`,
			backup,
			"systemctl --user restart agentfeedback.service",
		}
		notes = append(notes, `loginctl enable-linger "$USER" keeps the service running without a login session`,
			"after editing serve.env or running agentfeedback serve --init --force, run systemctl --user restart agentfeedback.service")
	case "launchd":
		home, err := os.UserHomeDir()
		if err != nil {
			return serverOutcome{}, errNoHome(err)
		}
		logDir = filepath.Join(home, "Library", "Logs", "agentfeedback")
		if err := checkSafePath("the log directory", logDir); err != nil {
			return serverOutcome{}, err
		}
		path = filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		data = launchdPlist(bin, env, filepath.Join(logDir, "serve.log"))
		next = []string{
			"launchctl bootstrap gui/$(id -u) " + path,
			"launchctl kickstart -k gui/$(id -u)/" + launchdLabel,
			"launchctl print gui/$(id -u)/" + launchdLabel,
			backup,
			"launchctl bootout gui/$(id -u)/" + launchdLabel + " && launchctl bootstrap gui/$(id -u) " + path,
		}
		notes = append(notes, "the plist copies serve.env: after editing serve.env run agentfeedback server install --launchd --force, then launchctl bootout gui/$(id -u)/"+launchdLabel+" and launchctl bootstrap gui/$(id -u) "+path)
	case "compose":
		if image == "" {
			v := clientVersion().Version
			if !isImageVersion(v) {
				return serverOutcome{}, errServerImageUnknown(v)
			}
			image = imageRepo + ":" + strings.TrimPrefix(v, "v")
		}
		// An image reference needs ":" for its tag or digest.
		if err := checkSafeRunes("--image", image, ":"); err != nil {
			return serverOutcome{}, err
		}
		path = filepath.Join(dir, "compose.yaml")
		parentMode = 0o700
		data = composeFile(image, env)
		next = []string{
			"docker compose -f " + path + " up -d",
			"docker compose -f " + path + " ps",
			"docker compose -f " + path + " logs -f agentfeedback",
			"docker compose -f " + path + " exec agentfeedback /opt/agentfeedback backup /data/backup-" + backupStamp + ".db",
			"docker compose -f " + path + " up -d --force-recreate",
		}
		notes = append(notes, "after editing serve.env or running agentfeedback serve --init --force, run docker compose -f "+path+" up -d --force-recreate: the key secret is mounted by inode")
	}
	if err := checkSafePath("the target path", path); err != nil {
		return serverOutcome{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), parentMode); err != nil {
		return serverOutcome{}, errServerWrite(path, err)
	}
	if err := placeFile(path, data, force); errors.Is(err, os.ErrExist) {
		return serverOutcome{}, errServerExists(path)
	} else if err != nil {
		return serverOutcome{}, errServerWrite(path, err)
	}
	// The log directory comes after the plist, so a refused run leaves nothing.
	if logDir != "" {
		if err := os.MkdirAll(logDir, 0o700); err != nil {
			return serverOutcome{}, errServerWrite(logDir, err)
		}
	}

	fmt.Fprintf(stderr, "agentfeedback server install: wrote %s; nothing was started\nnext:\n", path)
	for _, c := range next {
		fmt.Fprintf(stderr, "  %s\n", c)
	}
	for _, n := range notes {
		fmt.Fprintf(stderr, "note: %s\n", n)
	}

	return serverOutcome{Mode: mode, Path: path, Next: next}, nil
}

// isImageVersion reports whether v names a published image tag: a release,
// or a release candidate with prerelease exactly rc.N, and no build metadata.
func isImageVersion(v string) bool {
	sv, ok := parseVersion(v)
	if !ok || strings.Contains(v, "+") {
		return false
	}
	if len(sv.pre) == 0 {
		return true
	}
	if len(sv.pre) != 2 || sv.pre[0] != "rc" {
		return false
	}
	_, err := strconv.ParseUint(sv.pre[1], 10, 32)

	return err == nil
}

// xdgBase returns $<env> when it holds an absolute path, else ~/<fallback>.
func xdgBase(getenv func(string) string, env, fallback string) (string, error) {
	dir, err := xdgDir(getenv, env, fallback)
	if err != nil {
		return "", err
	}

	return filepath.Dir(dir), nil
}

// binaryPath is the running binary with symlinks resolved, so the unit keeps
// working when a symlink that pointed at it is moved.
func binaryPath() (string, error) {
	exe, err := executable()
	if err != nil {
		return "", errServerBinary(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", errServerBinary(err)
	}

	return exe, checkSafePath("the agentfeedback binary path", exe)
}

// readServeEnv parses the three KEY=VALUE lines serve --init writes; comment
// and blank lines are skipped, anything else is refused.
func readServeEnv(path string) (serveEnv, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return serveEnv{}, errServeEnvMissing(path)
	}
	if err != nil {
		return serveEnv{}, errPathUnreadable(path, err)
	}
	values := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		switch {
		case !ok:
			return serveEnv{}, errServeEnvInvalid(path, fmt.Sprintf("line %d is not KEY=VALUE", n))
		case k != "API_KEY_FILE" && k != "DATABASE_PATH" && k != "HTTP_LISTEN_ADDR":
			return serveEnv{}, errServeEnvInvalid(path, fmt.Sprintf("line %d sets %q, which server install does not know", n, k))
		case values[k] != "":
			return serveEnv{}, errServeEnvInvalid(path, fmt.Sprintf("line %d sets %s a second time", n, k))
		case v == "":
			return serveEnv{}, errServeEnvInvalid(path, fmt.Sprintf("line %d sets %s to nothing", n, k))
		}
		values[k] = v
	}
	if err := sc.Err(); err != nil {
		return serveEnv{}, errPathUnreadable(path, err)
	}
	for _, k := range []string{"API_KEY_FILE", "DATABASE_PATH", "HTTP_LISTEN_ADDR"} {
		if values[k] == "" {
			return serveEnv{}, errServeEnvInvalid(path, k+" is not set")
		}
	}
	env := serveEnv{keyFile: values["API_KEY_FILE"], database: values["DATABASE_PATH"], listen: values["HTTP_LISTEN_ADDR"]}
	for what, p := range map[string]string{"API_KEY_FILE": env.keyFile, "DATABASE_PATH": env.database} {
		if !filepath.IsAbs(p) {
			return serveEnv{}, errServeEnvInvalid(path, what+" is not an absolute path")
		}
		if err := checkSafePath(what, p); err != nil {
			return serveEnv{}, err
		}
	}
	host, port, err := net.SplitHostPort(env.listen)
	if err != nil {
		return serveEnv{}, errServeEnvInvalid(path, "HTTP_LISTEN_ADDR is not host:port")
	}
	if err := checkSafePath("the HTTP_LISTEN_ADDR host", host); err != nil {
		return serveEnv{}, err
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return serveEnv{}, errServeEnvInvalid(path, "HTTP_LISTEN_ADDR has no valid port")
	}
	// Normalised, so +8080 or 08080 never reaches the compose port mapping.
	env.port = strconv.Itoa(p)

	return env, nil
}

// systemdUnit is a user unit: the environment comes from serve.env, which
// names the key file, so the unit holds no key.
func systemdUnit(envPath, bin string) []byte {
	return fmt.Appendf(nil, `# written by agentfeedback server install --systemd
[Unit]
Description=AgentFeedback server

[Service]
Type=simple
EnvironmentFile=%s
ExecStart=%s serve
Restart=on-failure
NoNewPrivileges=yes
UMask=0077

[Install]
WantedBy=default.target
`, envPath, bin)
}

// launchdPlist copies the three serve.env values into the agent; the key
// stays in its file.
func launchdPlist(bin string, env serveEnv, logPath string) []byte {
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))

		return b.String()
	}

	return fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- written by agentfeedback server install --launchd -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>API_KEY_FILE</key>
		<string>%s</string>
		<key>DATABASE_PATH</key>
		<string>%s</string>
		<key>HTTP_LISTEN_ADDR</key>
		<string>%s</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, launchdLabel, esc(bin), esc(env.keyFile), esc(env.database), esc(env.listen), esc(logPath), esc(logPath))
}

// composeFile runs the image as the calling user, the key as a file secret,
// the database directory as /data, and publishes the port on loopback only.
func composeFile(image string, env serveEnv) []byte {
	return fmt.Appendf(nil, `# written by agentfeedback server install --compose
# The port is published on 127.0.0.1 only; exposing the server beyond this
# machine is the operator's decision.
services:
  agentfeedback:
    image: %s
    restart: unless-stopped
    stop_grace_period: 45s
    user: "%d:%d"
    environment:
      API_KEY_FILE: /run/secrets/api_key
      DATABASE_PATH: /data/%s
      HTTP_LISTEN_ADDR: 0.0.0.0:8080
    ports:
      - "127.0.0.1:%s:8080"
    volumes:
      - %s:/data
    secrets:
      - api_key
secrets:
  api_key:
    file: %s
`, image, os.Getuid(), os.Getgid(), filepath.Base(env.database), env.port, filepath.Dir(env.database), env.keyFile)
}
