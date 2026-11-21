// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web holds the Enable and Stop handlers of the operator page.
// Enable can arm a transmitter, so these guards apply to both handlers:
//
//   - Only POST changes state.
//   - The Host header must be the configured loopback address and port.
//   - The request must carry Sec-Fetch-Site same-origin or an Origin
//     equal to that address. A foreign Origin is always refused.
//   - The form must carry the token that the process created at start.
//
// The operator page renders the forms around these handlers. It puts
// the token into each form as the field TokenField.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
)

// Paths of the handlers on the page.
const (
	EnablePath = "/enable"
	StopPath   = "/stop"
)

// TokenField is the form field of the token.
const TokenField = "token"

// maxBody is the largest form that the handlers read.
const maxBody = 4096

// Controller is the gate. *tx.Gate implements it.
type Controller interface {
	// Enable arms the gate. It returns the reason when the gate refuses.
	Enable(ctx context.Context) error
	// Stop stops the gate on a request of the operator.
	Stop(ctx context.Context) error
}

// Handlers holds the Enable and Stop handlers and the form token.
type Handlers struct {
	ctrl   Controller
	host   string
	origin string
	token  string
}

// New builds the handlers. listen is the configured loopback address of
// the page, for example "127.0.0.1:8077". New creates a new random token.
func New(ctrl Controller, listen string) (*Handlers, error) {
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return nil, fmt.Errorf("web: page address %q: want host:port", listen)
	}
	return &Handlers{ctrl: ctrl, host: listen, origin: "http://" + listen, token: rand.Text()}, nil
}

// Token is the form token of this process. The page puts it into each
// form.
func (h *Handlers) Token() string { return h.token }

// Register adds the handlers to mux at EnablePath and StopPath.
func (h *Handlers) Register(mux *http.ServeMux) {
	mux.Handle(EnablePath, h.Enable())
	mux.Handle(StopPath, h.Stop())
}

// Enable gives the handler that arms the gate. On success it redirects
// to the page. When the gate refuses, it answers 409 with the reason.
func (h *Handlers) Enable() http.Handler {
	return h.guard(func(w http.ResponseWriter, r *http.Request) {
		if err := h.ctrl.Enable(r.Context()); err != nil {
			text(w, http.StatusConflict, "Enable refused: "+err.Error())
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}

// Stop gives the handler that stops the gate.
func (h *Handlers) Stop() http.Handler {
	return h.guard(func(w http.ResponseWriter, r *http.Request) {
		if err := h.ctrl.Stop(r.Context()); err != nil {
			text(w, http.StatusServiceUnavailable, "Stop failed: "+err.Error())
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}

// text writes a plain text answer that a browser does not read as HTML.
func text(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	fmt.Fprintln(w, msg)
}

// guard applies the checks before next.
func (h *Handlers) guard(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			text(w, http.StatusMethodNotAllowed, "only POST")
			return
		}
		if reason := h.check(w, r); reason != "" {
			text(w, http.StatusForbidden, "refused: "+reason)
			return
		}
		next(w, r)
	})
}

// check gives the reason to refuse r, or an empty string.
func (h *Handlers) check(w http.ResponseWriter, r *http.Request) string {
	if r.Host != h.host {
		return "wrong host"
	}
	origin := r.Header.Get("Origin")
	site := r.Header.Get("Sec-Fetch-Site")
	if origin != "" && origin != h.origin {
		return "foreign origin"
	}
	if site != "" && site != "same-origin" {
		return "not same origin"
	}
	if origin == "" && site == "" {
		return "no origin"
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		return "bad form"
	}
	got := r.PostForm.Get(TokenField)
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) != 1 {
		return "bad token"
	}
	return ""
}
