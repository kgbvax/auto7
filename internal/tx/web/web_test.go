// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const listen = "127.0.0.1:8077"

// fakeGate records the calls of the handlers.
type fakeGate struct {
	enables, stops int
	refuse         error
}

func (f *fakeGate) Enable(context.Context) error { f.enables++; return f.refuse }
func (f *fakeGate) Stop(context.Context) error   { f.stops++; return nil }

// post builds a POST as a browser on the page sends it. An empty token
// leaves the field out. mod can change the request.
func post(path, token string, mod func(*http.Request)) *http.Request {
	form := url.Values{}
	if token != "" {
		form.Set(TokenField, token)
	}
	r := httptest.NewRequest(http.MethodPost, "http://"+listen+path, strings.NewReader(form.Encode()))
	r.Host = listen
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://"+listen)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if mod != nil {
		mod(r)
	}
	return r
}

func serve(t *testing.T, h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func newHandlers(t *testing.T, g Controller) *Handlers {
	t.Helper()
	h, err := New(g, listen)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestEnableAccepted(t *testing.T) {
	g := &fakeGate{}
	h := newHandlers(t, g)
	w := serve(t, h.Enable(), post(EnablePath, h.Token(), nil))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" || g.enables != 1 {
		t.Fatalf("code %d location %q enables %d", w.Code, w.Header().Get("Location"), g.enables)
	}
}

func TestEnableGuards(t *testing.T) {
	tests := []struct {
		name  string
		token string
		mod   func(*http.Request)
		code  int
	}{
		{"GET", "", func(r *http.Request) { r.Method = http.MethodGet }, http.StatusMethodNotAllowed},
		{"foreign Origin", "", func(r *http.Request) {
			r.Header.Set("Origin", "http://evil.localhost")
			r.Header.Set("Sec-Fetch-Site", "cross-site")
		}, http.StatusForbidden},
		{"foreign Origin with same-origin claim", "", func(r *http.Request) {
			r.Header.Set("Origin", "http://evil.localhost")
		}, http.StatusForbidden},
		{"cross-site without Origin", "", func(r *http.Request) {
			r.Header.Del("Origin")
			r.Header.Set("Sec-Fetch-Site", "cross-site")
		}, http.StatusForbidden},
		{"no Origin and no Sec-Fetch-Site", "", func(r *http.Request) {
			r.Header.Del("Origin")
			r.Header.Del("Sec-Fetch-Site")
		}, http.StatusForbidden},
		{"wrong Host name", "", func(r *http.Request) { r.Host = "evil.localhost:8077" }, http.StatusForbidden},
		{"wrong Host port", "", func(r *http.Request) { r.Host = "127.0.0.1:8078" }, http.StatusForbidden},
		{"missing token", "-", nil, http.StatusForbidden},
		{"wrong token", "x", nil, http.StatusForbidden},
		{"token only in the URL", "-", func(r *http.Request) { r.URL.RawQuery = TokenField + "=" + "TOKEN" }, http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := &fakeGate{}
			h := newHandlers(t, g)
			token := h.Token()
			switch tc.token {
			case "-":
				token = ""
			case "x":
				token = h.Token() + "x"
			}
			r := post(EnablePath, token, tc.mod)
			if strings.Contains(r.URL.RawQuery, "TOKEN") {
				r.URL.RawQuery = TokenField + "=" + url.QueryEscape(h.Token())
			}
			w := serve(t, h.Enable(), r)
			if w.Code != tc.code || g.enables != 0 {
				t.Fatalf("code %d enables %d, want %d and 0", w.Code, g.enables, tc.code)
			}
		})
	}
}

// A browser sends one of the two headers. Each alone is enough.
func TestEnableWithOneOriginHeader(t *testing.T) {
	for _, del := range []string{"Origin", "Sec-Fetch-Site"} {
		t.Run("without "+del, func(t *testing.T) {
			g := &fakeGate{}
			h := newHandlers(t, g)
			w := serve(t, h.Enable(), post(EnablePath, h.Token(), func(r *http.Request) { r.Header.Del(del) }))
			if w.Code != http.StatusSeeOther || g.enables != 1 {
				t.Fatalf("code %d enables %d", w.Code, g.enables)
			}
		})
	}
}

func TestEnableRefusedWithTheReason(t *testing.T) {
	g := &fakeGate{refuse: errors.New("operator not present")}
	h := newHandlers(t, g)
	w := serve(t, h.Enable(), post(EnablePath, h.Token(), nil))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "operator not present") {
		t.Fatalf("code %d body %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("content type %q", ct)
	}
}

func TestStopIsGuardedToo(t *testing.T) {
	g := &fakeGate{}
	h := newHandlers(t, g)
	w := serve(t, h.Stop(), post(StopPath, "", nil))
	if w.Code != http.StatusForbidden || g.stops != 0 {
		t.Fatalf("stop without token: code %d stops %d", w.Code, g.stops)
	}
	w = serve(t, h.Stop(), post(StopPath, h.Token(), func(r *http.Request) { r.Header.Set("Origin", "http://evil.localhost") }))
	if w.Code != http.StatusForbidden || g.stops != 0 {
		t.Fatalf("stop with foreign Origin: code %d stops %d", w.Code, g.stops)
	}
	w = serve(t, h.Stop(), post(StopPath, h.Token(), nil))
	if w.Code != http.StatusSeeOther || g.stops != 1 {
		t.Fatalf("stop: code %d stops %d", w.Code, g.stops)
	}
}

func TestTokenPerProcess(t *testing.T) {
	a := newHandlers(t, &fakeGate{})
	b := newHandlers(t, &fakeGate{})
	if a.Token() == "" || a.Token() == b.Token() || len(a.Token()) < 20 {
		t.Fatalf("tokens %q and %q", a.Token(), b.Token())
	}
	g := &fakeGate{}
	h := newHandlers(t, g)
	if w := serve(t, h.Enable(), post(EnablePath, a.Token(), nil)); w.Code != http.StatusForbidden {
		t.Fatalf("token of another instance: code %d", w.Code)
	}
}

func TestRegister(t *testing.T) {
	g := &fakeGate{}
	h := newHandlers(t, g)
	mux := http.NewServeMux()
	h.Register(mux)
	serve(t, mux, post(EnablePath, h.Token(), nil))
	serve(t, mux, post(StopPath, h.Token(), nil))
	if g.enables != 1 || g.stops != 1 {
		t.Fatalf("enables %d stops %d", g.enables, g.stops)
	}
}

func TestNewRefusesBadAddress(t *testing.T) {
	if _, err := New(&fakeGate{}, "8077"); err == nil {
		t.Fatal("no error for an address without a host")
	}
}
