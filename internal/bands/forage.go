// SPDX-License-Identifier: AGPL-3.0-or-later

package bands

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/hunt"
	"github.com/kgbvax/auto7/internal/tx"
)

// Timing of the foraging.
const (
	// MinDwell is the shortest visit on a band: 8 FT8 periods or 16 FT4
	// periods. A band with the activity level "low_sample" gets exactly
	// this visit.
	MinDwell = 2 * time.Minute
	// SampleEvery is the interval of the samples of another band.
	SampleEvery = 20 * time.Minute
	// SpotWait is the time on the band of a high-value spot in which
	// auto7 must decode the station. Else the spot is "not heard here".
	SpotWait = 2 * time.Minute
	// ActivityEvery is the interval of the requests to horstreporter.
	ActivityEvery = 5 * time.Minute
)

// Rate model. The rate of a band and mode is in needed QSOs per hour.
const (
	// rateTau is the time constant of the moving average.
	rateTau = 30 * time.Minute
	// warmAfter is the listen time on a band and mode after which its
	// rate comes from the own QSOs. Before, the band is cold, and its rate
	// comes from the activity and the own decodes.
	warmAfter = 10 * time.Minute
	// coldRate is the rate of a cold band with normal activity.
	coldRate = 2.0
	// decodeShare is the share of the needed decodes of a cold band that
	// counts as QSOs.
	decodeShare = 0.25
	// passbandHz is the width above a dial in which a spot counts as a
	// spot of the mode of that dial.
	passbandHz = 3000
)

// Gate is the part of the TX gate that the forager uses. The gate decides
// each band change. The forager only reads its state.
type Gate interface {
	Submit(ctx context.Context, r tx.Request) tx.Decision
	State() tx.State
}

// Options holds the inputs of a Forager.
type Options struct {
	Gate Gate
	// Bands is the band list of the configuration.
	Bands []config.Band
	// FoxHound reports whether a spot lies on a Fox/Hound dial. auto7
	// never follows such a spot. Use Needs.SpotHighlightOnly. Nil means
	// no Fox/Hound dial.
	FoxHound func(bus.ChaseRow) bool
	// NotHeard is how long a spot stays "not heard here".
	NotHeard time.Duration
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Logger receives the log lines. Nil means slog.Default.
	Logger *slog.Logger
}

// key is one band and mode.
type key struct{ band, mode string }

// stats holds the results of one band and mode.
type stats struct {
	// measured is the listen time of the closed visits.
	measured time.Duration
	// avg is the moving average of the needed QSOs per hour.
	avg float64
	// decodes and needed count all decodes and the decodes of needed
	// stations, in the closed visits and the current visit.
	decodes, needed int
	// last is the end of the last visit. It is zero before the first.
	last time.Time
}

// visit is the current visit on a band and mode.
type visit struct {
	key   key
	start time.Time
	// qsos counts the logged QSOs with needed stations.
	qsos int
	// needed holds the needed stations that auto7 decoded in the visit.
	needed map[string]bool
}

// spotKey identifies a spot of a station on a band.
type spotKey struct{ call, band string }

// jump is a band change to a high-value spot that waits for the decode of
// the station.
type jump struct {
	spot     spotKey
	deadline time.Time
	// arrived is true when the visit on the band of the spot started.
	arrived bool
}

// Forager selects the band and mode. It hands band change requests to
// the gate. All methods are safe for use by many goroutines.
type Forager struct {
	gate     Gate
	bands    []config.Band
	foxHound func(bus.ChaseRow) bool
	notHeard time.Duration
	now      func() time.Time
	log      *slog.Logger

	mu       sync.Mutex
	stats    map[key]*stats
	visit    *visit
	activity map[string]Activity
	rows     []bus.ChaseRow
	// sampleAt is the time of the last sample, or of the first visit.
	sampleAt time.Time
	jump     *jump
	// silent holds the spots that are "not heard here", until the time.
	silent map[spotKey]time.Time
}

// New returns a Forager.
func New(o Options) *Forager {
	f := &Forager{
		gate: o.Gate, bands: o.Bands, foxHound: o.FoxHound, notHeard: o.NotHeard,
		now: o.Now, log: o.Logger,
		stats: map[key]*stats{}, silent: map[spotKey]time.Time{},
	}
	if f.now == nil {
		f.now = time.Now
	}
	if f.log == nil {
		f.log = slog.Default()
	}
	if f.foxHound == nil {
		f.foxHound = func(bus.ChaseRow) bool { return false }
	}
	return f
}

// SetChase records the last chase-queue state. Its rows carry the spots.
func (f *Forager) SetChase(cs bus.ChaseState) {
	f.mu.Lock()
	f.rows = slices.Clone(cs.Ranked)
	f.mu.Unlock()
}

// SetActivity records the band activity of horstreporter. Nil means that
// auto7 has no activity, for example after a failed request. Then the
// foraging uses the own decodes only.
func (f *Forager) SetActivity(a map[string]Activity) {
	f.mu.Lock()
	f.activity = a
	f.mu.Unlock()
}

// Observe counts one joined decode.
func (f *Forager) Observe(s hunt.Station) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := strings.ToUpper(s.Call)
	if j := f.jump; j != nil && j.spot == (spotKey{call, s.Band}) {
		// auto7 decoded the station of the spot.
		f.jump = nil
	}
	v := f.visit
	if v == nil || v.key != (key{s.Band, s.Mode}) {
		return
	}
	st := f.statsOf(v.key)
	st.decodes++
	if s.Needed {
		st.needed++
		v.needed[call] = true
	}
}

// Logged counts a logged QSO. It counts for the rate when auto7 decoded
// the station as needed in the current visit.
func (f *Forager) Logged(call, band, mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.visit
	if v != nil && v.key == (key{band, mode}) && v.needed[strings.ToUpper(call)] {
		v.qsos++
	}
}

// Spot is the spot hook of the task choice. It looks for a spot of a
// high-value station on another listed band. When it finds one, it hands
// one band change to the gate and returns true when the gate accepted
// it. A refused request returns false, and the next period tries again.
func (f *Forager) Spot(ctx context.Context, st tx.State) bool {
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.track(now, st)
	if st.Reduced() || st.Arm != tx.Running || f.jump != nil {
		return false
	}
	for _, r := range f.rows {
		if !hunt.HighValue(r) || r.Call == "" || f.foxHound(r) {
			continue
		}
		band := r.Band
		if r.FreqHz > 0 {
			if b := tx.BandOf(uint64(r.FreqHz)); b != "" {
				band = b
			}
		}
		sk := spotKey{strings.ToUpper(r.Call), band}
		if band == "" || band == st.Band || f.silent[sk].After(now) {
			continue
		}
		mode := f.spotMode(band, r.FreqHz)
		if mode == "" {
			continue
		}
		if d := f.gate.Submit(ctx, tx.BandChange{Band: band, Mode: mode}); !d.OK {
			return false
		}
		f.jump = &jump{spot: sk, deadline: now.Add(SpotWait)}
		f.log.Info("bands: band change to a high-value spot", "call", sk.call, "band", band, "mode", mode)
		return true
	}
	return false
}

// Period runs once per period, after the task choice. idle is true when
// the task choice found nothing to do on this band. Only then the forager
// leaves the band or samples another band. It hands at most one band
// change to the gate. A refused request waits for a later period.
func (f *Forager) Period(ctx context.Context, idle bool) {
	st := f.gate.State()
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.track(now, st)
	v := f.visit
	if st.Reduced() || v == nil || !idle || now.Sub(v.start) < MinDwell {
		return
	}
	var target key
	var why string
	switch {
	case now.Sub(f.sampleAt) >= SampleEvery:
		target, why = f.leastRecent(v.key), "sample"
	case !f.listed(v.key), f.lowSample(v.key.band), f.estimate(v.key, now) < f.average(now):
		target, why = f.best(v.key, now), "rate below the average"
	}
	if target == (key{}) {
		return
	}
	if d := f.gate.Submit(ctx, tx.BandChange{Band: target.band, Mode: target.mode}); !d.OK {
		return
	}
	if why == "sample" {
		f.sampleAt = now
	}
	f.log.Info("bands: band change", "band", target.band, "mode", target.mode, "reason", why)
}

// RunActivity asks horstreporter for the band activity every
// ActivityEvery until ctx ends. A failed request clears the activity, so
// the foraging uses the own decodes only.
func (f *Forager) RunActivity(ctx context.Context, c *Client) {
	for {
		a, err := c.Conditions(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			f.log.Warn("bands: horstreporter request failed: foraging uses the own decodes", "err", err)
		}
		f.SetActivity(a)
		select {
		case <-ctx.Done():
			return
		case <-time.After(ActivityEvery):
		}
	}
}

// track follows the visits from the gate state and ends a spot jump
// after SpotWait. A visit runs only while the gate runs. The caller holds
// mu.
func (f *Forager) track(now time.Time, st tx.State) {
	cur := key{st.Band, st.Mode}
	running := st.Arm == tx.Running && st.Band != ""
	if v := f.visit; v != nil && (!running || v.key != cur) {
		f.close(now)
	}
	if running && f.visit == nil {
		f.visit = &visit{key: cur, start: now, needed: map[string]bool{}}
		if f.sampleAt.IsZero() {
			f.sampleAt = now
		}
	}
	if j := f.jump; j != nil {
		if !j.arrived && f.visit != nil && f.visit.key.band == j.spot.band {
			j.arrived, j.deadline = true, f.visit.start.Add(SpotWait)
		}
		if !now.Before(j.deadline) {
			f.silent[j.spot] = now.Add(f.notHeard)
			f.jump = nil
			f.log.Info("bands: spot not heard here", "call", j.spot.call, "band", j.spot.band)
		}
	}
	for sk, until := range f.silent {
		if !until.After(now) {
			delete(f.silent, sk)
		}
	}
}

// close ends the current visit and folds its rate into the moving
// average. The caller holds mu.
func (f *Forager) close(now time.Time) {
	v := f.visit
	f.visit = nil
	d := now.Sub(v.start)
	st := f.statsOf(v.key)
	prior := f.prior(v.key, d)
	st.avg = fold(prior, float64(v.qsos)/d.Hours(), d)
	st.measured += d
	st.last = now
}

// fold moves the average prior towards rate by the weight of a visit of
// length d.
func fold(prior, rate float64, d time.Duration) float64 {
	if d <= 0 {
		return prior
	}
	w := 1 - math.Exp(-float64(d)/float64(rateTau))
	return prior + w*(rate-prior)
}

// prior gives the rate of k before the current visit. extra is the time
// of the current visit on k. A cold band gets its rate from the activity
// and the own decodes. The caller holds mu.
func (f *Forager) prior(k key, extra time.Duration) float64 {
	st := f.statsOf(k)
	if st.measured >= warmAfter {
		return st.avg
	}
	ratio := 1.0
	if a, ok := f.activity[k.band]; ok && a.trusted() {
		ratio = a.Ratio
	}
	r := coldRate * ratio
	if h := (st.measured + extra).Hours(); h > 0 {
		r += decodeShare * float64(st.needed) / h
	}
	return r
}

// estimate gives the rate of k now, with the current visit. The caller
// holds mu.
func (f *Forager) estimate(k key, now time.Time) float64 {
	v := f.visit
	if v == nil || v.key != k {
		return f.prior(k, 0)
	}
	d := now.Sub(v.start)
	if d <= 0 {
		return f.prior(k, 0)
	}
	return fold(f.prior(k, d), float64(v.qsos)/d.Hours(), d)
}

// keys lists every band and mode of the band list for which WSJT-X has
// a default dial.
func (f *Forager) keys() []key {
	var out []key
	for _, b := range f.bands {
		for _, m := range b.Modes {
			if _, ok := tx.DefaultDial(b.Name, m); ok {
				out = append(out, key{b.Name, m})
			}
		}
	}
	return out
}

func (f *Forager) listed(k key) bool { return slices.Contains(f.keys(), k) }

// average gives the average rate of all listed bands and modes. The
// caller holds mu.
func (f *Forager) average(now time.Time) float64 {
	ks := f.keys()
	if len(ks) == 0 {
		return 0
	}
	sum := 0.0
	for _, k := range ks {
		sum += f.estimate(k, now)
	}
	return sum / float64(len(ks))
}

// lowSample reports whether horstreporter has too few reports for band.
func (f *Forager) lowSample(band string) bool {
	a, ok := f.activity[band]
	return ok && a.Level == LevelLowSample
}

// best gives the listed band and mode other than cur with the highest
// rate. Of equal rates, the one with the oldest visit wins. The caller
// holds mu.
func (f *Forager) best(cur key, now time.Time) key {
	var out key
	var top float64
	for _, k := range f.keys() {
		if k == cur {
			continue
		}
		r := f.estimate(k, now)
		if out == (key{}) || r > top || (r == top && f.statsOf(k).last.Before(f.statsOf(out).last)) {
			out, top = k, r
		}
	}
	return out
}

// leastRecent gives the listed band and mode other than cur with the
// oldest visit. A band and mode without a visit comes first. The caller
// holds mu.
func (f *Forager) leastRecent(cur key) key {
	var out key
	for _, k := range f.keys() {
		if k == cur {
			continue
		}
		if out == (key{}) || f.statsOf(k).last.Before(f.statsOf(out).last) {
			out = k
		}
	}
	return out
}

// spotMode gives the mode for a jump to a spot on band. A spot in the
// passband of a default dial gets the mode of that dial. Else the mode
// with the most own decodes per hour on the band wins. It is empty when
// the band is not in the band list. The caller holds mu.
func (f *Forager) spotMode(band string, freqHz int64) string {
	var modes []string
	for _, b := range f.bands {
		if b.Name == band {
			modes = b.Modes
		}
	}
	for _, m := range modes {
		if d, ok := tx.DefaultDial(band, m); ok && freqHz >= int64(d) && freqHz < int64(d)+passbandHz {
			return m
		}
	}
	out, top := "", -1.0
	for _, m := range modes {
		if _, ok := tx.DefaultDial(band, m); !ok {
			continue
		}
		st := f.statsOf(key{band, m})
		r := 0.0
		if h := st.measured.Hours(); h > 0 {
			r = float64(st.decodes) / h
		}
		if r > top {
			out, top = m, r
		}
	}
	return out
}

func (f *Forager) statsOf(k key) *stats {
	st, ok := f.stats[k]
	if !ok {
		st = &stats{}
		f.stats[k] = st
	}
	return st
}
