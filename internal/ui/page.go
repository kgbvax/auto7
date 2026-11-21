// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ui renders the operator page. The page shows the state of
// auto7 around the Enable and Stop forms. The handlers of these forms
// live in internal/tx/web. The page listens only on a loopback address.
// It runs no script, and html/template escapes every text, because some
// texts come from the internet.
package ui

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/kgbvax/auto7/internal/tx"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// ContentSecurityPolicy goes with every response. It allows no script,
// the style sheet of the page only, and form posts to the page only.
const ContentSecurityPolicy = "default-src 'none'; script-src 'none'; style-src 'self'; img-src 'self'; " +
	"form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

// RefreshSeconds is the interval of the page reload.
const RefreshSeconds = 5

// StylePath is the path of the style sheet.
const StylePath = "/style.css"

//go:embed page.html
var pageHTML string

//go:embed style.css
var styleCSS []byte

// View is the state that the page shows.
type View struct {
	// State is the state of the TX gate.
	State tx.State
	// Task describes the current task of the hunt.
	Task string
	// ListsErr is the error of the local lists. It is empty when the
	// lists are readable.
	ListsErr string
}

// Form names the parts of the Enable and Stop forms.
type Form struct {
	// EnablePath and StopPath are the paths of the handlers.
	EnablePath, StopPath string
	// TokenField is the name of the token field, and Token its value.
	TokenField, Token string
}

// Option is one WSJT-X setting that auto7 needs.
type Option struct {
	Name  string
	Value string
}

// Options lists the WSJT-X settings that auto7 needs.
var Options = []Option{
	{"Hold Tx Freq", "on"},
	{"Double-click on call sets Tx enable", "on"},
	{"Auto Seq", "on"},
	{"Call 1st", "off"},
	{"Prompt me to log QSO", "off"},
	{"Tx watchdog", "short"},
}

// Page serves the operator page at "/" and its style sheet.
type Page struct {
	tmpl *template.Template
	form Form
	view func() View
}

// New builds the page. view gives the state at each request.
func New(form Form, view func() View) (*Page, error) {
	t, err := template.New("page").Parse(pageHTML)
	if err != nil {
		return nil, fmt.Errorf("ui: parse page: %w", err)
	}
	return &Page{tmpl: t, form: form, view: view}, nil
}

// pageData is the data of the template.
type pageData struct {
	View
	Form
	Armed    bool
	ArmText  string
	Reduced  bool
	Build    string
	Options  []Option
	Refresh  int
	Stopped  string
	Refusal  string
	Presence string
}

func (p *Page) data() pageData {
	v := p.view()
	s := v.State
	d := pageData{
		View:     v,
		Form:     p.form,
		Armed:    s.Arm.Armed(),
		Reduced:  s.Reduced(),
		Build:    buildText(s.Build),
		Options:  Options,
		Refresh:  RefreshSeconds,
		Presence: s.PresenceHost,
	}
	switch s.Arm {
	case tx.Running:
		d.ArmText = "Enabled"
	case tx.Settling:
		d.ArmText = "Enabled, waiting for WSJT-X on the new band"
	default:
		d.ArmText = "Stopped"
	}
	if !s.StoppedAt.IsZero() {
		d.Stopped = s.StoppedAt.UTC().Format(time.TimeOnly) + " UTC"
	}
	if r := s.LastRefusal; r.Reason != "" {
		d.Refusal = r.Kind + ": " + r.Reason
	}
	return d
}

// ServeHTTP serves the page and the style sheet. Other paths give 404.
func (p *Page) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "only GET", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/":
	case StylePath:
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write(styleCSS)
		return
	default:
		http.NotFound(w, r)
		return
	}
	var b bytes.Buffer
	if err := p.tmpl.Execute(&b, p.data()); err != nil {
		http.Error(w, "page error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b.Bytes())
}

// Secure sets the Content-Security-Policy and the other safety headers
// on every response of h.
func Secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", ContentSecurityPolicy)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

// Listen opens the TCP listener of the page. The host must be a loopback
// address or localhost, and localhost binds to 127.0.0.1.
func Listen(addr string) (net.Listener, error) {
	bind, err := loopback(addr)
	if err != nil {
		return nil, err
	}
	return net.Listen("tcp", bind)
}

// loopback gives the bind address for addr. It refuses every host that
// is not a loopback address or localhost.
func loopback(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("ui: page address %q: want host:port", addr)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	a, err := netip.ParseAddr(host)
	if err != nil || !a.Unmap().IsLoopback() {
		return "", fmt.Errorf("ui: page address %q: want a loopback address", addr)
	}
	return net.JoinHostPort(host, port), nil
}

// buildText names the WSJT-X build for the page.
func buildText(b wsjtx.Build) string {
	if b == wsjtx.BuildFull {
		return "full (ws-hunter)"
	}
	return "reduced (stock WSJT-X)"
}
