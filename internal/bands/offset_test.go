// SPDX-License-Identifier: AGPL-3.0-or-later

package bands

import (
	"context"
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/tx"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// slot0 is the start of the first test period since midnight UTC.
const slot0 = 12 * time.Hour

// pick returns a random source that always gives index k of n, or the
// last index when k is too large.
func pick(k int) func(int) int {
	return func(n int) int { return min(k, n-1) }
}

func newOffsets(clk *clock, gate *fakeGate, r func(int) int) *Offsets {
	return NewOffsets(OffsetOptions{
		Gate:   gate,
		Limits: config.Limits{OffsetMovesPerHour: 6, OffsetMinDistanceHz: 50},
		Now:    clk.now,
		Rand:   r,
	})
}

func fullGate() *fakeGate {
	return &fakeGate{st: tx.State{Arm: tx.Running, Build: wsjtx.BuildFull, Band: "20m", Mode: "FT8"}}
}

// moves returns the offset moves since the last call.
func (g *fakeGate) moves() []uint32 {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []uint32
	for _, r := range g.reqs {
		if m, ok := r.(tx.OffsetMove); ok {
			out = append(out, m.TxDF)
		}
	}
	g.reqs = nil
	return out
}

// busy decodes a station every 50 Hz from 200 Hz to 4000 Hz in one
// period, except in the open ranges (lo, hi) of gaps.
func busy(o *Offsets, period time.Duration, gaps ...[2]uint32) {
	for df := uint32(200); df <= 4000; df += 50 {
		skip := false
		for _, g := range gaps {
			if df > g[0] && df < g[1] {
				skip = true
			}
		}
		if !skip {
			o.Observe(wsjtx.Decode{New: true, Time: period, DeltaFreq: df})
		}
	}
}

// choices runs Choose with many random indexes and returns each result.
func choices(t *testing.T, o *Offsets, avoid ...uint32) []uint32 {
	t.Helper()
	var out []uint32
	for k := 0; k < 4000; k += 37 {
		o.rand = pick(k)
		df, ok := o.Choose(avoid...)
		if !ok {
			t.Fatalf("index %d: no clear offset", k)
		}
		out = append(out, df)
	}
	return out
}

func TestChosenOffsetLiesInTheOnlyClearGap(t *testing.T) {
	o := newOffsets(&clock{t: t0}, fullGate(), nil)
	// Stations every 50 Hz, with one 100 Hz gap from 1000 to 1100 Hz.
	busy(o, slot0, [2]uint32{1000, 1100})
	for _, df := range choices(t, o) {
		if df <= 1000 || df >= 1100 {
			t.Fatalf("chosen offset %d Hz, want it inside the gap from 1000 to 1100 Hz", df)
		}
	}
}

func TestChosenOffsetIsNeverNearTheCalledStation(t *testing.T) {
	o := newOffsets(&clock{t: t0}, fullGate(), nil)
	for _, df := range choices(t, o, 1500) {
		if df > 1450 && df < 1550 {
			t.Fatalf("chosen offset %d Hz lies within 50 Hz of the called station at 1500 Hz", df)
		}
	}
	// The called station sits in the middle of the only gap.
	busy(o, slot0, [2]uint32{1000, 1200})
	for _, df := range choices(t, o, 1100) {
		if df != 1050 && df != 1150 {
			t.Fatalf("chosen offset %d Hz, want 1050 or 1150 Hz", df)
		}
	}
}

func TestChosenOffsetStaysInsideTheRange(t *testing.T) {
	o := newOffsets(&clock{t: t0}, fullGate(), nil)
	for _, k := range []int{0, 1 << 30} {
		o.rand = pick(k)
		df, ok := o.Choose()
		if !ok || df < 200 || df > 4000 {
			t.Fatalf("index %d: offset %d Hz (ok %v), want 200 to 4000 Hz", k, df, ok)
		}
	}
}

func TestSpectrumMarksSignalsThatGaveNoDecode(t *testing.T) {
	o := newOffsets(&clock{t: t0}, fullGate(), nil)
	// Bins of 10 Hz from 200 Hz to 4000 Hz. Only the noise is in the
	// bins, except a signal 20 dB above the noise from 1000 to 1050 Hz.
	levels := make([]byte, 381)
	for i := range levels {
		levels[i] = 4
		if hz := 200 + 10*i; hz >= 1000 && hz <= 1050 {
			levels[i] = 44
		}
	}
	o.Spectrum(wsjtx.SpectrumSummary{PeriodStart: slot0, StartHz: 200, BinWidthHz: 10, FloorDB: -30, StepDB: 0.5, Levels: levels})
	for _, df := range choices(t, o) {
		if df > 950 && df < 1100 {
			t.Fatalf("chosen offset %d Hz lies within 50 Hz of the signal from 1000 to 1050 Hz", df)
		}
	}
}

func TestOldPeriodsAreForgotten(t *testing.T) {
	o := newOffsets(&clock{t: t0}, fullGate(), nil)
	busy(o, slot0, [2]uint32{1000, 1100})
	// Two newer periods hear nothing near 3000 Hz. The busy period is
	// older than both, so its stations no longer count.
	o.Observe(wsjtx.Decode{New: true, Time: slot0 + 15*time.Second, DeltaFreq: 600})
	o.Observe(wsjtx.Decode{New: true, Time: slot0 + 30*time.Second, DeltaFreq: 700})
	o.rand = pick(1 << 30)
	if df, _ := o.Choose(); df != 4000 {
		t.Fatalf("chosen offset %d Hz, want 4000 Hz after the busy period expired", df)
	}
}

func TestSeventhHopWithinOneHourIsNotSent(t *testing.T) {
	clk := &clock{t: t0}
	g := fullGate()
	o := newOffsets(clk, g, pick(0))
	ctx := context.Background()
	// Each minute a station appears on the own offset, so each minute a
	// hop is due.
	for i := range 7 {
		clk.set(t0.Add(time.Duration(i) * time.Minute))
		own := uint32(1000 + 100*i)
		o.SetStatus(wsjtx.Status{DialHz: 14_074_000, TxDF: own})
		o.Observe(wsjtx.Decode{New: true, Time: slot0 + time.Duration(i)*15*time.Second, DeltaFreq: own})
		o.Hop(ctx, g.State())
		got := g.moves()
		if i < 6 && len(got) != 1 {
			t.Fatalf("hop %d: %d requests, want 1", i+1, len(got))
		}
		if i == 6 && len(got) != 0 {
			t.Fatalf("hop 7 within one hour: %d requests, want none", len(got))
		}
	}
	// One hour after the first hop, a hop is possible again.
	clk.set(t0.Add(time.Hour))
	o.Hop(ctx, g.State())
	if got := g.moves(); len(got) != 1 {
		t.Fatalf("hop after one hour: %d requests, want 1", len(got))
	}
}

func TestHopWaitsForTheIntervalWithoutAClash(t *testing.T) {
	clk := &clock{t: t0}
	g := fullGate()
	o := newOffsets(clk, g, pick(0))
	ctx := context.Background()
	o.SetStatus(wsjtx.Status{DialHz: 14_074_000, TxDF: 2500})
	// The first hop chooses a free offset at once.
	o.Hop(ctx, g.State())
	if got := g.moves(); len(got) != 1 {
		t.Fatalf("first hop: %d requests, want 1", len(got))
	}
	// Six moves per hour give one move each 10 minutes.
	clk.set(t0.Add(9 * time.Minute))
	o.Hop(ctx, g.State())
	if got := g.moves(); len(got) != 0 {
		t.Fatalf("hop after 9 minutes: %d requests, want none", len(got))
	}
	clk.set(t0.Add(10 * time.Minute))
	o.Hop(ctx, g.State())
	got := g.moves()
	if len(got) != 1 {
		t.Fatalf("hop after 10 minutes: %d requests, want 1", len(got))
	}
	if got[0] > 2450 && got[0] < 2550 {
		t.Fatalf("hop to %d Hz stays near the own offset 2500 Hz", got[0])
	}
}

func TestNoHopDuringAQSO(t *testing.T) {
	g := fullGate()
	g.st.QSO = tx.QSOView{Open: true, DXCall: dxCall}
	o := newOffsets(&clock{t: t0}, g, pick(0))
	o.Hop(context.Background(), g.State())
	if got := g.moves(); len(got) != 0 {
		t.Fatalf("hop during a QSO: %d requests, want none", len(got))
	}
}

func TestNoHopInReducedModeOrWhenStopped(t *testing.T) {
	for _, st := range []tx.State{
		{Arm: tx.Running, Build: wsjtx.BuildReduced, Band: "20m"},
		{Arm: tx.Stopped, Build: wsjtx.BuildFull, Band: "20m"},
	} {
		g := &fakeGate{st: st}
		o := newOffsets(&clock{t: t0}, g, pick(0))
		o.Hop(context.Background(), g.State())
		if got := g.moves(); len(got) != 0 {
			t.Fatalf("state %+v: %d hop requests, want none", st, len(got))
		}
	}
}
