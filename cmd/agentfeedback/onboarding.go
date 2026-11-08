package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

const (
	loginSynopsis = "login [--email EMAIL] [--server cloud|URL] [--json]"
	claimNotice   = "This installation is unclaimed and has reduced attribution until you claim it."
)

type onboardingChoice struct {
	email string
	agent bool
	enrol bool
}

type onboardingCredentials struct {
	key            string
	claimURL       string
	claimExpiresAt string
}

func onboardingMachine() (string, error) {
	path, err := configPath(os.Getenv)
	if err != nil {
		return "", err
	}
	file, _, err := loadFileConfig(path)
	if err != nil {
		return "", err
	}
	machine := resolveClient(flagConfig{}, os.Getenv, file).Machine.Value
	if machine == "" {
		machine, err = os.Hostname()
		machine, _, _ = strings.Cut(machine, ".")
	}
	if err != nil || strings.TrimSpace(machine) == "" {
		return "", errors.New("cannot determine this machine's name")
	}
	return machine, nil
}

func onboardingEmail(email string) (string, error) {
	email = strings.TrimSpace(email)
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || strings.ContainsAny(email, "\r\n") {
		return "", usageErr("a valid email address is required", "pass --email EMAIL to login or --login EMAIL to init")
	}
	return email, nil
}

func promptOnboardingEmail(in *bufio.Reader, stderr io.Writer) (string, error) {
	fmt.Fprint(stderr, "Email address: ")
	email, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", errors.New("could not read email address")
	}
	return onboardingEmail(email)
}

func maskedEmail(email string) string {
	_, domain, _ := strings.Cut(email, "@")
	return "***@" + domain
}

func chooseOnboarding(ctx context.Context, c *client.OnboardingClient, server string, in *bufio.Reader, stderr io.Writer) (onboardingChoice, error) {
	caps, err := c.Capabilities(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "Warning: could not discover authentication options; paste an installation key to continue.")
	}
	fmt.Fprintf(stderr, "How do you want to authenticate with %s? [1] paste an installation key (Enter)", server)
	if caps.Email {
		fmt.Fprint(stderr, " [2] sign in or register by email")
	}
	if caps.Agent {
		fmt.Fprint(stderr, " [3] start without an account (claim later)")
	}
	fmt.Fprint(stderr, ": ")
	answer, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return onboardingChoice{}, errors.New("could not read authentication choice")
	}
	switch strings.TrimSpace(answer) {
	case "", "1":
		return onboardingChoice{}, nil
	case "2":
		if caps.Email {
			email, err := promptOnboardingEmail(in, stderr)
			return onboardingChoice{email: email}, err
		}
	case "3":
		if caps.Agent {
			return onboardingChoice{agent: true}, nil
		}
	}
	return onboardingChoice{}, usageErr("authentication choice is not available", "choose one of the displayed options")
}

func acquireOnboarding(ctx context.Context, c *client.OnboardingClient, choice onboardingChoice, stdin io.Reader, stderr io.Writer) (onboardingCredentials, error) {
	caps, err := c.Capabilities(ctx)
	if err != nil {
		return onboardingCredentials{}, err
	}
	switch {
	case choice.email != "" && !caps.Email:
		return onboardingCredentials{}, usageErr("this server offers no email sign-in", "use --key-from-stdin with an installation key")
	case choice.agent && !caps.Agent:
		return onboardingCredentials{}, usageErr("this server offers no agent self-enrolment", "use --key-from-stdin with an installation key")
	case choice.enrol && !caps.Enrol:
		return onboardingCredentials{}, usageErr("this server offers no enrolment-token exchange", "use --key-from-stdin with an installation key")
	}
	// Discover unsupported methods without local writes, but check persistence
	// before consuming a one-time token or asking the user to approve a device.
	path, err := configPath(os.Getenv)
	if err != nil {
		return onboardingCredentials{}, err
	}
	unlock, err := lockConfigFile(path)
	if err != nil {
		return onboardingCredentials{}, errInitWrite(path, err)
	}
	probe, err := os.CreateTemp(filepath.Dir(path), ".onboarding-write-check-*")
	if err == nil {
		err = probe.Close()
		if removeErr := os.Remove(probe.Name()); err == nil {
			err = removeErr
		}
	}
	unlock()
	if err != nil {
		return onboardingCredentials{}, errInitWrite(path, err)
	}
	machine, err := onboardingMachine()
	if err != nil {
		return onboardingCredentials{}, err
	}
	switch {
	case choice.email != "":
		email, err := onboardingEmail(choice.email)
		if err != nil {
			return onboardingCredentials{}, err
		}
		key, err := loginDevice(ctx, c, email, machine, stderr)
		return onboardingCredentials{key: key}, err
	case choice.agent:
		result, err := c.SelfEnrol(ctx, machine)
		return onboardingCredentials{key: result.Key, claimURL: result.ClaimURL, claimExpiresAt: result.ClaimExpiresAt}, err
	case choice.enrol:
		token, err := readKey(stdin, "init --enrol-token-from-stdin")
		if err != nil {
			return onboardingCredentials{}, err
		}
		key, err := c.Enrol(ctx, token, machine)
		var response *client.OnboardingError
		if errors.As(err, &response) && response.Status == 401 {
			err = errors.New("the enrolment token is invalid, expired or revoked")
		}
		return onboardingCredentials{key: key}, err
	}
	return onboardingCredentials{}, errors.New("no authentication method selected")
}

func loginDevice(ctx context.Context, c *client.OnboardingClient, email, machine string, stderr io.Writer) (string, error) {
	device, err := c.Device(ctx, email, machine)
	if err != nil {
		return "", err
	}
	// The verification URL and device code are capabilities, not terminal output.
	fmt.Fprintf(stderr, "Check your email (%s) and approve this machine (link valid %s). Ctrl-C cancels.\n", maskedEmail(email), device.ExpiresIn)
	ctx, cancel := context.WithTimeout(ctx, device.ExpiresIn)
	defer cancel()
	interval := device.Interval
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", errors.New("email login expired; retry with agentfeedback login --email EMAIL --server URL")
			}
			return "", errors.New("email login cancelled")
		case <-timer.C:
		}
		key, err := c.DeviceToken(ctx, device.DeviceCode)
		if err == nil {
			return key, nil
		}
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", errors.New("email login expired; retry with agentfeedback login --email EMAIL --server URL")
			}
			return "", errors.New("email login cancelled")
		}
		var response *client.OnboardingError
		if !errors.As(err, &response) {
			return "", err
		}
		switch response.Code {
		case "authorization_pending":
		case "slow_down":
			// Bound addition by the remaining authorization lifetime to avoid overflow.
			if interval < device.ExpiresIn && device.ExpiresIn-interval > 5*time.Second {
				interval += 5 * time.Second
			} else {
				interval = device.ExpiresIn
			}
		case "access_denied":
			return "", errors.New("email login denied")
		case "expired_token":
			return "", errors.New("email login expired; retry with agentfeedback login --email EMAIL --server URL")
		default:
			return "", err
		}
	}
}

func runLogin(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("login")
	email := fs.String("email", "", "email address to sign in or register")
	server := fs.String("server", "", "configured server: cloud or a base URL")
	asJSON := fs.Bool("json", false, "print one JSON result")
	fs.Usage = func() { fmt.Fprintf(fs.Output(), "usage: agentfeedback %s\n", loginSynopsis); fs.PrintDefaults() }
	pos, err := parseInterleaved(fs, args, stderr)
	if err == nil && len(pos) != 0 {
		err = errArgs("login", loginSynopsis)
	}
	if err != nil {
		return errFlags("login", err)
	}
	res := initResult{}
	finish := func(err error) error { return finishInit(stdout, res, *asJSON, err) }
	path, err := configPath(os.Getenv)
	if err != nil {
		return finish(err)
	}
	file, _, err := loadFileConfig(path)
	if err != nil {
		return finish(err)
	}
	configured := resolveClient(flagConfig{}, os.Getenv, file).URL.Value
	if configured == "" {
		return finish(usageErr("login needs a configured server", "run init --server cloud --login EMAIL first"))
	}
	current, err := normaliseServer("configured server", configured)
	if err != nil {
		return finish(err)
	}
	if current == serverLocal {
		return finish(usageErr("login needs a configured remote server", "run init --server cloud --login EMAIL first"))
	}
	if *server == "" {
		*server = configured
	}
	target, err := normaliseServer("--server", *server)
	if err != nil {
		return finish(err)
	}
	if current != target {
		return finish(usageErr("login cannot change the configured server", "use the configured server"))
	}
	if err := checkLoginDestination(file.URL, target); err != nil {
		return finish(err)
	}
	if os.Getenv(envAPIKey) != "" {
		return finish(usageErr("an environment API key overrides the saved credentials", "unset "+envAPIKey+" before login"))
	}
	if *email == "" && isTerminal() {
		*email, err = promptOnboardingEmail(bufio.NewReader(stdin), stderr)
	} else {
		*email, err = onboardingEmail(*email)
	}
	if err != nil {
		return finish(err)
	}
	c, err := client.NewOnboardingClient(target, nil)
	if err != nil {
		return finish(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	credentials, err := acquireOnboarding(ctx, c, onboardingChoice{email: *email}, stdin, stderr)
	if err != nil {
		return finish(err)
	}
	if err = updateLoginCredentials(path, target, credentials.key); err != nil {
		return finish(err)
	}
	res.Status, res.Mode, res.Server, res.Config = "authenticated", modeRemote, target, path
	if *asJSON {
		return finish(nil)
	}
	_, err = fmt.Fprintf(stdout, "Signed in. Installation credentials saved to %s.\n", path)
	return err
}
