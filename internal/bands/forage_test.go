// SPDX-License-Identifier: AGPL-3.0-or-later

package bands

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/hunt"
	"github.com/kgbvax/auto7/internal/tx"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// Invented calls of the tests.
const (
	ownCall = "XY9ZZ"
	expCall = "XX2BB" // a first-tier expedition
	dxCall  = "XX1AA" // needed, not high value
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// fakeGate records the requests. refuse gives the reason of a refusal,
// or "" to accept.
type fakeGate struct {
	mu     sync.Mutex
	st     tx.State
	reqs   []tx.Request
	refuse func(tx.Request) string
}

func (g *fakeGate) Submit(_ context.Context, r tx.Request) tx.Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reqs = append(g.reqs, r)
	if g.refuse != nil {
		if reason := g.refuse(r); reason != "" {
			return tx.Decision{Reason: reason}
		}
	}
	return tx.Decision{OK: true}
}

func (g *fakeGate) State() tx.State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.st
}

func (g *fakeGate) on(band, mode string) {
	g.mu.Lock()
	g.st.Band, g.st.Mode = band, mode
	g.mu.Unlock()
}

// take returns the band changes since the last take.
func (g *fakeGate) take() []tx.BandChange {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []tx.BandChange
	for _, r := range g.reqs {
		if b, ok := r.(tx.BandChange); ok {
			out = append(out, b)
		}
	}
	g.reqs = nil
	return out
}

type env struct {
	t    *testing.T
	clk  *clock
	gate *fakeGate
	f    *Forager
	log  *bytes.Buffer
}

func ft8(names ...string) []config.Band {
	var out []config.Band
	for _, n := range names {
		out = append(out, config.Band{Name: n, Modes: []string{config.ModeFT8}})
	}
	return out
}

func newEnv(t *testing.T, bands []config.Band, foxHound func(bus.ChaseRow) bool) *env {
	clk := &clock{t: t0}
	g := &fakeGate{st: tx.State{Arm: tx.Running, Build: wsjtx.BuildFull, Band: "20m", Mode: config.ModeFT8}}
	buf := &bytes.Buffer{}
	f := New(Options{
		Gate: g, Bands: bands, FoxHound: foxHound, NotHeard: 30 * time.Minute,
		Now: clk.now, Logger: slog.New(slog.NewTextHandler(buf, nil)),
	})
	return &env{t: t, clk: clk, gate: g, f: f, log: buf}
}

// at sets the clock to t0 plus d.
func (e *env) at(d time.Duration) { e.clk.set(t0.Add(d)) }

// period runs one period at t0 plus d.
func (e *env) period(d time.Duration, idle bool) []tx.BandChange {
	e.at(d)
	e.f.Period(context.Background(), idle)
	return e.gate.take()
}

// spot runs the spot hook at t0 plus d.
func (e *env) spot(d time.Duration) (bool, []tx.BandChange) {
	e.at(d)
	ok := e.f.Spot(context.Background(), e.gate.State())
	return ok, e.gate.take()
}

func wantChange(t *testing.T, step string, got []tx.BandChange, want ...tx.BandChange) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: band changes %v, want %v", step, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: band changes %v, want %v", step, got, want)
		}
	}
}

func expedition(band string, freqHz int64) bus.ChaseRow {
	return bus.ChaseRow{Call: expCall, Band: band, Needed: true, FreqHz: freqHz, Source: "cluster",
		Expedition: &bus.ChaseExpedition{Call: expCall, Tier: bus.ExpeditionTierFirst}}
}

func station(call, band string, needed bool) hunt.Station {
	return hunt.Station{Call: call, Band: band, Mode: config.ModeFT8, Needed: needed}
}

// A first-tier expedition on 17 m while a QSO runs on 20 m: the task
// choice sends no band change during the QSO. After the QSO, the change
// to 17 m goes out.
func TestHighValueSpotWaitsForTheEndOfTheQSO(t *testing.T) {
	e := newEnv(t, ft8("20m", "17m"), nil)
	lists, err := hunt.OpenLists(t.TempDir(), 10*time.Minute, e.clk.now)
	if err != nil {
		t.Fatal(err)
	}
	limits := config.Limits{Tries: 4, TriesHighValue: 12, SkipMinutes: 10}
	ch := hunt.NewChooser(e.gate, lists, limits, func() string { return ownCall })
	ch.SetSpotHook(e.f.Spot)
	e.f.SetChase(bus.ChaseState{Ranked: []bus.ChaseRow{expedition("17m", 18_100_500)}})

	e.gate.st.QSO = tx.QSOView{Open: true, DXCall: dxCall, Band: "20m"}
	for i := range 4 {
		e.at(time.Duration(i) * 15 * time.Second)
		if task := ch.Run(context.Background()); task.Kind != hunt.TaskQSO {
			t.Fatalf("period %d: task %s, want QSO", i, task.Kind)
		}
		wantChange(t, "during the QSO", e.gate.take())
	}

	e.gate.st.QSO = tx.QSOView{}
	e.at(time.Minute)
	if task := ch.Run(context.Background()); task.Kind != hunt.TaskSpot {
		t.Fatalf("after the QSO: task %s, want %s", task.Kind, hunt.TaskSpot)
	}
	wantChange(t, "after the QSO", e.gate.take(), tx.BandChange{Band: "17m", Mode: config.ModeFT8})
}

// A first-tier expedition on 14090 kHz, a Fox/Hound dial: no band change.
// The same spot on 14074 kHz gets one.
func TestNoBandChangeToASpotOnAFoxHoundDial(t *testing.T) {
	lists, err := hunt.OpenLists(t.TempDir(), 10*time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	needs := hunt.NewNeeds(lists, []uint64{14_090_000})
	e := newEnv(t, ft8("40m", "20m"), needs.SpotHighlightOnly)
	e.gate.on("40m", config.ModeFT8)

	e.f.SetChase(bus.ChaseState{Ranked: []bus.ChaseRow{expedition("20m", 14_090_000)}})
	ok, got := e.spot(0)
	if ok {
		t.Error("the hook reports a band change for a Fox/Hound spot")
	}
	wantChange(t, "Fox/Hound spot", got)

	e.f.SetChase(bus.ChaseState{Ranked: []bus.ChaseRow{expedition("20m", 14_075_000)}})
	ok, got = e.spot(15 * time.Second)
	if !ok {
		t.Error("the hook reports no band change for a normal spot")
	}
	wantChange(t, "normal spot", got, tx.BandChange{Band: "20m", Mode: config.ModeFT8})
}

// A band below the average rate leaves after the minimum dwell, and not
// before. A refused request goes out again in the next period, once.
func TestBandBelowTheAverageLeavesAfterTheMinimumDwell(t *testing.T) {
	e := newEnv(t, ft8("20m", "17m"), nil)
	e.f.SetActivity(map[string]Activity{
		"20m": {Band: "20m", Ratio: 0.5, Level: LevelBelow},
		"17m": {Band: "17m", Ratio: 2.0, Level: LevelAbove},
	})
	wantChange(t, "start", e.period(0, true))
	wantChange(t, "after 1 min", e.period(time.Minute, true))
	wantChange(t, "busy after 2 min", e.period(2*time.Minute, false))

	e.gate.refuse = func(tx.Request) string { return tx.ReasonTransmits }
	want := tx.BandChange{Band: "17m", Mode: config.ModeFT8}
	wantChange(t, "refused", e.period(2*time.Minute+15*time.Second, true), want)
	e.gate.refuse = nil
	wantChange(t, "next period", e.period(2*time.Minute+30*time.Second, true), want)
}

// A band above the average stays after the minimum dwell.
func TestBandAboveTheAverageStays(t *testing.T) {
	e := newEnv(t, ft8("20m", "17m"), nil)
	e.f.SetActivity(map[string]Activity{
		"20m": {Band: "20m", Ratio: 2.0, Level: LevelAbove},
		"17m": {Band: "17m", Ratio: 0.5, Level: LevelBelow},
	})
	e.period(0, true)
	for m := 2; m < 20; m++ {
		wantChange(t, "above the average", e.period(time.Duration(m)*time.Minute, true))
	}
}

// A band with the activity level "low_sample" gets the shortest visit: it
// leaves after the minimum dwell, while the same band with the level
// "normal" stays.
func TestLowSampleBandGetsTheShortestVisit(t *testing.T) {
	for _, tc := range []struct {
		level string
		want  []tx.BandChange
	}{
		{LevelNormal, nil},
		{LevelLowSample, []tx.BandChange{{Band: "17m", Mode: config.ModeFT8}}},
	} {
		t.Run(tc.level, func(t *testing.T) {
			e := newEnv(t, ft8("20m", "17m"), nil)
			e.f.SetActivity(map[string]Activity{
				"20m": {Band: "20m", Ratio: 1.0, Level: tc.level},
				"17m": {Band: "17m", Ratio: 0.5, Level: LevelBelow},
			})
			e.period(0, true)
			wantChange(t, "before the minimum dwell", e.period(MinDwell-time.Second, true))
			wantChange(t, "at the minimum dwell", e.period(MinDwell, true), tc.want...)
		})
	}
}

// A failed horstreporter request clears the activity. The foraging goes
// on with the own decodes: 17 m had needed decodes, 20 m had none, and
// 15 m had no visit.
func TestFailedHorstreporterRequestLeavesTheOwnDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	e := newEnv(t, ft8("20m", "17m", "15m"), nil)
	e.gate.on("17m", config.ModeFT8)
	e.period(0, false)
	e.f.Observe(station(dxCall, "17m", true))
	e.f.Observe(station("XX3CC", "17m", true))
	e.f.Observe(station("XX4DD", "17m", false))
	e.gate.on("20m", config.ModeFT8)
	e.period(4*time.Minute, false)

	// With the activity, 20 m is far above normal and stays.
	e.f.SetActivity(map[string]Activity{
		"20m": {Band: "20m", Ratio: 10, Level: LevelAbove},
		"17m": {Band: "17m", Ratio: 0.1, Level: LevelBelow},
		"15m": {Band: "15m", Ratio: 0.1, Level: LevelBelow},
	})
	wantChange(t, "with the activity", e.period(6*time.Minute, true))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.f.RunActivity(ctx, NewClient(config.HorstreporterConf{URL: srv.URL, QTH: "JJ00"}, nil))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.f.mu.Lock()
		cleared := e.f.activity == nil
		e.f.mu.Unlock()
		if cleared {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the failed request did not clear the activity")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	wantChange(t, "after the failed request", e.period(6*time.Minute+15*time.Second, true),
		tx.BandChange{Band: "17m", Mode: config.ModeFT8})
}

// A band visit samples another band every 20 minutes, also when the band
// is above the average.
func TestSampleAnotherBandEvery20Minutes(t *testing.T) {
	e := newEnv(t, ft8("20m", "17m", "15m"), nil)
	e.f.SetActivity(map[string]Activity{
		"20m": {Band: "20m", Ratio: 3.0, Level: LevelAbove},
		"17m": {Band: "17m", Ratio: 1.0, Level: LevelNormal},
		"15m": {Band: "15m", Ratio: 1.0, Level: LevelNormal},
	})
	e.gate.on("15m", config.ModeFT8)
	e.period(0, false)
	e.gate.on("20m", config.ModeFT8)
	e.period(time.Minute, false)
	wantChange(t, "before 20 min", e.period(20*time.Minute-time.Second, true))
	// 17 m had no visit, 15 m had one: the sample goes to 17 m.
	wantChange(t, "at 20 min", e.period(20*time.Minute, true), tx.BandChange{Band: "17m", Mode: config.ModeFT8})
}

// A high-value spot that auto7 does not decode within 2 minutes on its
// band is "not heard here" for 30 minutes. Then the spot gets a jump
// again.
func TestSpotNotDecodedIsNotHeardHereFor30Minutes(t *testing.T) {
	e := newEnv(t, ft8("20m", "17m"), nil)
	e.f.SetChase(bus.ChaseState{Ranked: []bus.ChaseRow{expedition("17m", 18_101_000)}})
	jump := tx.BandChange{Band: "17m", Mode: config.ModeFT8}
	if _, got := e.spot(0); len(got) != 1 || got[0] != jump {
		t.Fatalf("first spot: %v, want %v", got, jump)
	}
	// The gate settles, and the visit on 17 m starts at 30 s.
	e.gate.st.Arm = tx.Settling
	e.period(15*time.Second, false)
	e.gate.st.Arm = tx.Running
	e.gate.on("17m", config.ModeFT8)
	e.period(30*time.Second, false)
	e.f.Observe(station(dxCall, "17m", true))
	e.period(2*time.Minute+25*time.Second, false)
	e.mustNotBeSilent("before 2 min on the band")
	e.period(2*time.Minute+30*time.Second, false)

	// auto7 leaves 17 m. The spot is still on chase-queue.
	e.gate.on("20m", config.ModeFT8)
	e.period(3*time.Minute, false)
	if ok, got := e.spot(4 * time.Minute); ok || len(got) != 0 {
		t.Fatalf("spot not heard here: %v", got)
	}
	if ok, got := e.spot(32*time.Minute + 29*time.Second); ok || len(got) != 0 {
		t.Fatalf("spot 1 s before the end of the 30 min: %v", got)
	}
	if _, got := e.spot(32*time.Minute + 30*time.Second); len(got) != 1 || got[0] != jump {
		t.Fatalf("spot after 30 min: %v, want %v", got, jump)
	}
}

func (e *env) mustNotBeSilent(step string) {
	e.t.Helper()
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	if len(e.f.silent) != 0 {
		e.t.Fatalf("%s: spots not heard here %v", step, e.f.silent)
	}
}

// A spot station that auto7 decodes on its band is not marked.
func TestDecodedSpotIsNotMarked(t *testing.T) {
	e := newEnv(t, ft8("20m", "17m"), nil)
	e.f.SetChase(bus.ChaseState{Ranked: []bus.ChaseRow{expedition("17m", 18_101_000)}})
	e.spot(0)
	e.gate.on("17m", config.ModeFT8)
	e.period(30*time.Second, false)
	e.f.Observe(station(expCall, "17m", true))
	e.period(5*time.Minute, false)
	e.mustNotBeSilent("after the decode")
}

// The mode of a jump to a spot off the default dials comes from the own
// decode counts of the band visits: FT4 had more decodes than FT8.
func TestSpotModeComesFromTheOwnDecodes(t *testing.T) {
	both := []config.Band{
		{Name: "20m", Modes: []string{config.ModeFT8}},
		{Name: "17m", Modes: []string{config.ModeFT8, config.ModeFT4}},
	}
	e := newEnv(t, both, nil)
	visit := func(start time.Duration, mode string, decodes int) {
		e.gate.on("17m", mode)
		e.period(start, false)
		for range decodes {
			e.f.Observe(hunt.Station{Call: dxCall, Band: "17m", Mode: mode})
		}
	}
	visit(0, config.ModeFT8, 2)
	visit(3*time.Minute, config.ModeFT4, 9)
	e.gate.on("20m", config.ModeFT8)
	e.period(6*time.Minute, false)

	e.f.SetChase(bus.ChaseState{Ranked: []bus.ChaseRow{expedition("17m", 18_130_000)}})
	_, got := e.spot(7 * time.Minute)
	wantChange(t, "spot off the dials", got, tx.BandChange{Band: "17m", Mode: config.ModeFT4})

	// A spot in the passband of the FT8 dial gets FT8.
	e2 := newEnv(t, both, nil)
	e2.f.SetChase(bus.ChaseState{Ranked: []bus.ChaseRow{expedition("17m", 18_101_200)}})
	_, got = e2.spot(0)
	wantChange(t, "spot on the FT8 dial", got, tx.BandChange{Band: "17m", Mode: config.ModeFT8})
}

// With stock WSJT-X the gate refuses band changes. The forager sends no
// request and logs nothing.
func TestReducedModeSendsNoBandChangeAndLogsNothing(t *testing.T) {
	e := newEnv(t, ft8("20m", "17m"), nil)
	e.gate.st.Build = wsjtx.BuildReduced
	e.f.SetActivity(map[string]Activity{"20m": {Band: "20m", Ratio: 1.0, Level: LevelLowSample}})
	e.f.SetChase(bus.ChaseState{Ranked: []bus.ChaseRow{expedition("17m", 18_101_000)}})
	for i := range 200 {
		d := time.Duration(i) * 15 * time.Second
		if ok, got := e.spot(d); ok || len(got) != 0 {
			t.Fatalf("spot hook at %s: %v", d, got)
		}
		wantChange(t, "period", e.period(d, true))
	}
	if e.log.Len() != 0 {
		t.Errorf("log lines in reduced mode:\n%s", e.log.String())
	}
}
