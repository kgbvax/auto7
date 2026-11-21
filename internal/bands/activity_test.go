// SPDX-License-Identifier: AGPL-3.0-or-later

package bands

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kgbvax/auto7/internal/config"
)

func TestConditionsReadsTheActivityOfEachBand(t *testing.T) {
	var gotPath, gotQTH string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQTH = r.URL.Path, r.URL.Query().Get("qth")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"overall_score": 50, "bands": [
			{"band": "20m", "activity_ratio": 1.8, "activity_level": "above", "spots_per_minute": 12},
			{"band": "17m", "activity_ratio": 0.2, "activity_level": "low_sample"}
		]}`))
	}))
	defer srv.Close()

	c := NewClient(config.HorstreporterConf{URL: srv.URL + "/", QTH: "JJ00"}, nil)
	got, err := c.Conditions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/dx_conditions" || gotQTH != "JJ00" {
		t.Errorf("request %s with qth %q, want /api/dx_conditions with qth JJ00", gotPath, gotQTH)
	}
	want := map[string]Activity{
		"20m": {Band: "20m", Ratio: 1.8, Level: LevelAbove},
		"17m": {Band: "17m", Ratio: 0.2, Level: LevelLowSample},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for b, a := range want {
		if got[b] != a {
			t.Errorf("%s: got %+v, want %+v", b, got[b], a)
		}
	}
}

func TestConditionsFailsOnAnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := NewClient(config.HorstreporterConf{URL: srv.URL, QTH: "JJ00"}, nil)
	if _, err := c.Conditions(context.Background()); err == nil {
		t.Fatal("no error for status 503")
	}
}

func TestNoClientWithoutAURL(t *testing.T) {
	if c := NewClient(config.HorstreporterConf{}, nil); c != nil {
		t.Fatal("client without a URL")
	}
}
