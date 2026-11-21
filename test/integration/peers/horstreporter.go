// SPDX-License-Identifier: AGPL-3.0-or-later

package peers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
)

// Horstreporter is a fake horstreporter HTTP server on a loopback port.
// It answers each path with the body that the test set. While it fails, it
// answers every request with status 503.
type Horstreporter struct {
	srv *httptest.Server

	mu       sync.Mutex
	bodies   map[string][]byte
	failing  bool
	requests []string
}

// NewHorstreporter starts the fake.
func NewHorstreporter() *Horstreporter {
	h := &Horstreporter{bodies: map[string][]byte{}}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	return h
}

// URL is the base URL of the fake.
func (h *Horstreporter) URL() string { return h.srv.URL }

// SetResponse makes the fake answer the path, for example
// "/api/dx_conditions", with v as JSON.
func (h *Horstreporter) SetResponse(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.bodies[path] = b
	h.mu.Unlock()
	return nil
}

// SetFailing makes every request fail with status 503 while on is true.
func (h *Horstreporter) SetFailing(on bool) {
	h.mu.Lock()
	h.failing = on
	h.mu.Unlock()
}

// Requests returns the request URIs that the fake received, oldest first.
func (h *Horstreporter) Requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...)
}

// Close stops the server.
func (h *Horstreporter) Close() { h.srv.Close() }

func (h *Horstreporter) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.requests = append(h.requests, r.URL.RequestURI())
	failing := h.failing
	body, ok := h.bodies[r.URL.Path]
	h.mu.Unlock()
	switch {
	case failing:
		http.Error(w, "failing on request of the test", http.StatusServiceUnavailable)
	case !ok:
		http.NotFound(w, r)
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}
