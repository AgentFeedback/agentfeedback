// Package ui is the read-only web page over the queue that agentfeedback ui
// serves on loopback: the queue, one record with its recomputed warnings,
// the stats and a sessions placeholder. It reads through the v1 API client
// only (GET), never mounts the API or /mcp, and answers nothing unless the
// request names the bound loopback host, carries no foreign Origin and
// starts its path with the launch token.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

// Source is what the page reads from: pkg/client's Do. The handler only
// ever passes GET.
type Source interface {
	Do(ctx context.Context, method, path string, query url.Values, body []byte) (*client.Response, error)
}

// Config configures the handler.
type Config struct {
	// Source answers the API requests.
	Source Source
	// Token is the first path segment every request must carry.
	Token string
	// Addr is the bound loopback address, host:port; requests must name it
	// or localhost:<port> in Host.
	Addr string
	// Mode is the line the nav shows: "local" or the server's host.
	Mode string
	// Now is the clock the since presets count back from; nil is time.Now.
	Now func() time.Time
}

// CSP is the Content-Security-Policy of every response: same-origin
// scripts, styles and images only, no framing, no remote source.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

//go:embed templates/*.html static/*
var assets embed.FS

// NewToken returns 32 random bytes, base64url without padding.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(b), nil
}

type handler struct {
	src     Source
	token   string
	hosts   []string
	origins []string
	mode    string
	now     func() time.Time
	pages   map[string]*template.Template
}

// New returns the handler for cfg.
func New(cfg Config) (http.Handler, error) {
	if cfg.Source == nil {
		return nil, errors.New("ui: no source")
	}
	if len(cfg.Token) < 16 {
		return nil, errors.New("ui: token too short")
	}
	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("ui: address %q: %w", cfg.Addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("ui: address %q is not a loopback IP", cfg.Addr)
	}
	if p, err := strconv.Atoi(port); err != nil || p <= 0 {
		return nil, fmt.Errorf("ui: address %q has no bound port", cfg.Addr)
	}
	hosts := []string{net.JoinHostPort(ip.String(), port), net.JoinHostPort("localhost", port)}
	h := &handler{src: cfg.Source, token: cfg.Token, hosts: hosts, mode: cfg.Mode, now: cfg.Now}
	for _, a := range hosts {
		h.origins = append(h.origins, "http://"+a)
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.pages, err = parsePages(); err != nil {
		return nil, err
	}

	return h, nil
}

// parsePages parses each page template together with the layout.
func parsePages() (map[string]*template.Template, error) {
	pages := map[string]*template.Template{}
	for _, name := range []string{"queue", "record", "stats", "sessions", "error"} {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("ui: template %s: %w", name, err)
		}
		pages[name] = t
	}

	return pages, nil
}

// setHeaders sets the headers every response carries, errors included.
func setHeaders(w http.ResponseWriter) {
	hd := w.Header()
	hd.Set("Cache-Control", "no-store")
	hd.Set("Content-Security-Policy", CSP)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "no-referrer")
}

// ServeHTTP checks, in order, the Host, the Origin, the token and the
// method, then routes the rest of the path.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setHeaders(w)
	if !contains(h.hosts, r.Host) {
		plain(w, r, http.StatusForbidden, "this page answers only on its loopback address")

		return
	}
	if o, ok := r.Header["Origin"]; ok && (len(o) != 1 || !contains(h.origins, o[0])) {
		plain(w, r, http.StatusForbidden, "cross-origin requests are refused")

		return
	}
	rest, ok := h.strip(r.URL.Path)
	if !ok {
		plain(w, r, http.StatusNotFound, "not found")

		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		h.fail(w, r, http.StatusMethodNotAllowed, "this page is read-only")

		return
	}
	switch {
	case rest == "":
		h.queue(w, r)
	case rest == "stats":
		h.stats(w, r)
	case rest == "sessions":
		h.render(w, r, http.StatusOK, "sessions", page{Title: "Sessions"})
	case rest == "static/app.css":
		h.static(w, r, "app.css", "text/css; charset=utf-8")
	case rest == "static/app.js":
		h.static(w, r, "app.js", "text/javascript; charset=utf-8")
	case strings.HasPrefix(rest, "submissions/"):
		s := strings.TrimPrefix(rest, "submissions/")
		id, err := strconv.ParseInt(s, 10, 64)
		if !recordID.MatchString(s) || err != nil {
			h.fail(w, r, http.StatusNotFound, "not found")

			return
		}
		h.record(w, r, id)
	default:
		h.fail(w, r, http.StatusNotFound, "not found")
	}
}

// strip returns the path after "/<token>/", comparing the token in
// constant time.
func (h *handler) strip(path string) (string, bool) {
	n := len(h.token)
	if len(path) < n+2 || path[0] != '/' || path[n+1] != '/' {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(path[1:n+1]), []byte(h.token)) != 1 {
		return "", false
	}

	return path[n+2:], true
}

// recordID is the record route's id, as the API's pathID accepts it.
var recordID = regexp.MustCompile(`^[1-9][0-9]*$`)

// plain answers a request refused before the token is verified: a short
// text body that carries nothing of the token, the security headers kept.
func plain(w http.ResponseWriter, r *http.Request, status int, msg string) {
	body := msg + "\n"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}

	return false
}

func (h *handler) static(w http.ResponseWriter, r *http.Request, name, ctype string) {
	b, err := fs.ReadFile(assets, "static/"+name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)

		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

// get issues one GET through the source.
func (h *handler) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	res, err := h.src.Do(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return nil, err
	}

	return res.Body, nil
}

// upstream renders a failed API request: a transport error is 502, an API
// error keeps its status, anything else is 500.
func (h *handler) upstream(w http.ResponseWriter, r *http.Request, err error) {
	var ae *client.APIError
	var te *client.TransportError
	switch {
	case errors.As(err, &ae):
		msg := ae.Message
		if msg == "" {
			msg = ae.Error()
		}
		status := ae.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		h.fail(w, r, status, msg)
	case errors.As(err, &te):
		h.fail(w, r, http.StatusBadGateway, "the API is unreachable: "+te.Error())
	default:
		h.fail(w, r, http.StatusInternalServerError, err.Error())
	}
}

// fail renders the error page with status and msg.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	h.render(w, r, status, "error", page{Title: http.StatusText(status), Data: errorView{Status: status, Message: msg}})
}

// page is what the layout reads; Data is the page's own view.
type page struct {
	Title string
	Base  string
	Mode  string
	Nav   string
	Data  any
}

func (h *handler) render(w http.ResponseWriter, r *http.Request, status int, name string, p page) {
	p.Base = "/" + h.token + "/"
	p.Mode = h.mode
	p.Nav = name
	var b strings.Builder
	if err := h.pages[name].Execute(&b, p); err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("template error\n"))

		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(b.String()))
	}
}

type errorView struct {
	Status  int
	Message string
}
