// SPDX-License-Identifier: AGPL-3.0-or-later

// Package bands moves auto7 between the bands of the band list. It keeps a
// rate of needed QSOs per band and mode, reads the band activity of
// horstreporter, and hands band change requests to the TX gate. The gate
// decides each request.
package bands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kgbvax/auto7/internal/config"
)

// Activity levels of horstreporter. They compare the activity of a band
// with its own normal.
const (
	LevelAbove      = "above"
	LevelNormal     = "normal"
	LevelBelow      = "below"
	LevelLowSample  = "low_sample"
	LevelNoBaseline = "no_baseline"
)

// Activity is the activity of one band from horstreporter.
type Activity struct {
	Band string
	// Ratio is the activity of the band against its normal. 1 is normal.
	Ratio float64
	// Level is one of the Level constants.
	Level string
}

// trusted reports whether Ratio carries a real comparison. horstreporter
// gives a real ratio only with the levels above, normal and below.
func (a Activity) trusted() bool {
	switch a.Level {
	case LevelAbove, LevelNormal, LevelBelow:
		return true
	}
	return false
}

// requestTimeout bounds one request to horstreporter.
const requestTimeout = 10 * time.Second

// maxBody bounds the answer of horstreporter.
const maxBody = 4 << 20

// Client reads the band activity from horstreporter. It only reads.
type Client struct {
	base string
	qth  string
	http *http.Client
}

// NewClient returns a client for the configured horstreporter. It returns
// nil when the configuration has no URL. A nil hc uses a client with a
// timeout of 10 s.
func NewClient(c config.HorstreporterConf, hc *http.Client) *Client {
	if c.URL == "" {
		return nil
	}
	if hc == nil {
		hc = &http.Client{Timeout: requestTimeout}
	}
	return &Client{base: strings.TrimRight(c.URL, "/"), qth: c.QTH, http: hc}
}

// conditions is the part of the answer of /api/dx_conditions that auto7
// reads.
type conditions struct {
	Bands []struct {
		Band          string  `json:"band"`
		ActivityRatio float64 `json:"activity_ratio"`
		ActivityLevel string  `json:"activity_level"`
	} `json:"bands"`
}

// Conditions asks /api/dx_conditions for the activity of each band. The
// map key is the band name, for example "20m".
func (c *Client) Conditions(ctx context.Context) (map[string]Activity, error) {
	u := c.base + "/api/dx_conditions?" + url.Values{"qth": {c.qth}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("horstreporter: status %d", resp.StatusCode)
	}
	var body conditions
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body); err != nil {
		return nil, fmt.Errorf("horstreporter: %w", err)
	}
	out := make(map[string]Activity, len(body.Bands))
	for _, b := range body.Bands {
		if b.Band == "" {
			continue
		}
		out[b.Band] = Activity{Band: b.Band, Ratio: b.ActivityRatio, Level: b.ActivityLevel}
	}
	return out, nil
}
