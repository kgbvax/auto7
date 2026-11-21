// SPDX-License-Identifier: AGPL-3.0-or-later

package ui

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kgbvax/auto7/internal/tx"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

var testForm = Form{EnablePath: "/enable", StopPath: "/stop", TokenField: "token", Token: "tok-123"}

func newPage(t *testing.T, v View) http.Handler {
	t.Helper()
	p, err := New(testForm, func() View { return v })
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", p)
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "refused", http.StatusForbidden)
	})
	return Secure(mux)
}

func get(t *testing.T, h http.Handler, method, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, "http://127.0.0.1:8077"+path, nil))
	return rec.Result()
}

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestScriptInTextShowsAsPlainText(t *testing.T) {
	const evil = `<script>alert("x")</script>`
	h := newPage(t, View{
		Task:     "hunt XX1AA " + evil,
		ListsErr: evil,
		State: tx.State{
			StopReason:  evil,
			Hint:        evil,
			Warning:     evil,
			Band:        evil,
			LastRefusal: tx.Refusal{Kind: "call", Reason: evil},
		},
	})
	r := get(t, h, http.MethodGet, "/")
	b := body(t, r)
	if strings.Contains(strings.ToLower(b), "<script") {
		t.Fatalf("page holds a script element:\n%s", b)
	}
	if n := strings.Count(b, "&lt;script&gt;"); n < 7 {
		t.Errorf("escaped script text shows %d times, want at least 7", n)
	}
}

func TestEveryResponseCarriesTheCSP(t *testing.T) {
	h := newPage(t, View{})
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/"},
		{http.MethodGet, StylePath},
		{http.MethodGet, "/favicon.ico"},
		{http.MethodPost, "/"},
		{http.MethodPost, "/stop"},
	} {
		r := get(t, h, c.method, c.path)
		csp := r.Header.Get("Content-Security-Policy")
		if csp != ContentSecurityPolicy {
			t.Errorf("%s %s: CSP %q", c.method, c.path, csp)
		}
		// With no-referrer a browser sends "Origin: null" on the form
		// post, and the Enable and Stop handlers refuse it.
		if rp := r.Header.Get("Referrer-Policy"); rp != "same-origin" {
			t.Errorf("%s %s: Referrer-Policy %q, want same-origin", c.method, c.path, rp)
		}
	}
	for _, part := range []string{"script-src 'none'", "default-src 'none'"} {
		if !strings.Contains(ContentSecurityPolicy, part) {
			t.Errorf("CSP lacks %q", part)
		}
	}
}

func TestPageListensOnlyOnLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.0.2.1:0", "10.1.2.3:0", "[::]:0", "example.org:0"} {
		if got, err := loopback(addr); err == nil {
			t.Errorf("%s: accepted as %s", addr, got)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "localhost:0"} {
		ln, err := Listen(addr)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		host, _, _ := net.SplitHostPort(ln.Addr().String())
		ln.Close()
		if host != "127.0.0.1" {
			t.Errorf("%s: bound to %s", addr, host)
		}
	}
}

func TestPageShowsReducedModeWithStockWSJTX(t *testing.T) {
	b := body(t, get(t, newPage(t, View{State: tx.State{Build: wsjtx.BuildReduced}}), http.MethodGet, "/"))
	if !strings.Contains(b, "reduced (stock WSJT-X)") || !strings.Contains(b, "Reduced mode:") {
		t.Errorf("page does not show reduced mode:\n%s", b)
	}
	b = body(t, get(t, newPage(t, View{State: tx.State{Build: wsjtx.BuildFull}}), http.MethodGet, "/"))
	if strings.Contains(b, "Reduced mode:") || !strings.Contains(b, "full (ws-hunter)") {
		t.Errorf("page with a ws-hunter build:\n%s", b)
	}
}

func TestPageShowsStateFormsAndOptions(t *testing.T) {
	h := newPage(t, View{
		Task: "hunt XX1AA, try 2",
		State: tx.State{
			Arm: tx.Running, Band: "20m", Mode: "FT8", PresenceHost: "shack-pc",
			StopReason: "operator not present",
		},
	})
	b := body(t, get(t, h, http.MethodGet, "/"))
	for _, want := range []string{
		"<strong>Enabled</strong>", "20m", "FT8", "hunt XX1AA, try 2", "shack-pc", "operator not present",
		`action="/enable"`, `action="/stop"`, `name="token" value="tok-123"`,
		"Hold Tx Freq", "Double-click on call sets Tx enable", "Auto Seq", "Call 1st",
		"Prompt me to log QSO", "Tx watchdog", "short",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Count(b, `name="token"`) != 2 {
		t.Error("each form needs the token")
	}
	b = body(t, get(t, newPage(t, View{}), http.MethodGet, "/"))
	if !strings.Contains(b, "<strong>Stopped</strong>") || !strings.Contains(b, "operator not present") {
		t.Errorf("stopped page:\n%s", b)
	}
}

func TestStyleSelectsNoTextOutsideFields(t *testing.T) {
	h := newPage(t, View{})
	css := body(t, get(t, h, http.MethodGet, StylePath))
	for _, want := range []string{"user-select: none", "-webkit-user-select: none", ".selectable", "user-select: text"} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css lacks %q", want)
		}
	}
	b := body(t, get(t, newPage(t, View{State: tx.State{StopReason: "operator not present", LastRefusal: tx.Refusal{Kind: "call", Reason: "late"}}}), http.MethodGet, "/"))
	if n := strings.Count(b, `class="selectable"`); n < 2 {
		t.Errorf("page has %d selectable cells, want the stop reason and the last refusal", n)
	}
}
