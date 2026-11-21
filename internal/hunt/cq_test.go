// SPDX-License-Identifier: AGPL-3.0-or-later

package hunt

import (
	"context"
	"testing"

	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/tx"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// expedition is an invented first-tier expedition with a spot on 17 m.
const expedition = "XY6FF"

// cqBands is the band list of the CQ tests.
var cqBands = []config.Band{{Name: "20m", Modes: []string{"FT8"}}, {Name: "17m", Modes: []string{"FT8"}}}

// newCQEnv returns an env with CQ limits, the spot check of the needs and
// an offset hook that records each call. rows replace the chase-queue
// rows of newEnv.
func newCQEnv(t *testing.T, rows ...bus.ChaseRow) (*env, *[]string) {
	t.Helper()
	e := newEnv(t)
	e.f.needs.SetChase(chase(rows...))
	e.ch.limits.CQMaxPeriods, e.ch.limits.CQListenPeriods = 10, 4
	e.ch.SetSpotCheck(func() bool { return e.f.needs.HighValueSpot(cqBands) })
	var hops []string
	e.ch.SetOffsetHook(func(_ context.Context, st tx.State) bool {
		hops = append(hops, "hop")
		return true
	})
	return e, &hops
}

// cqOf returns the CQ requests among reqs.
func cqOf(reqs []tx.Request) []tx.CQ {
	var out []tx.CQ
	for _, r := range reqs {
		if c, ok := r.(tx.CQ); ok {
			out = append(out, c)
		}
	}
	return out
}

func expeditionRow(band string) bus.ChaseRow {
	return bus.ChaseRow{Call: expedition, Band: band, Needed: true,
		Expedition: &bus.ChaseExpedition{Call: expedition, Tier: "first"}}
}

func TestDirectedCQWhenASpotIsNotHeardHere(t *testing.T) {
	// The forager marked the expedition on 17 m "not heard here", so the
	// spot hook does not follow it. The band has no needed station and no
	// caller.
	e, hops := newCQEnv(t, bus.ChaseRow{Call: normal, Band: "20m", Needed: true}, expeditionRow("17m"))
	e.ch.SetSpotHook(func(context.Context, tx.State) bool { return false })
	task, reqs := e.run("CQ " + other + " AA00")
	cqs := cqOf(reqs)
	if len(cqs) != 1 || !cqs[0].Directed || cqs[0].Periods != 10 {
		t.Fatalf("requests %q, want one directed CQ of 10 periods", describe(reqs))
	}
	if task.Kind != TaskCQ || !task.Requested || !task.Decision.OK {
		t.Fatalf("task %+v, want an accepted CQ", task)
	}
	if len(*hops) != 1 {
		t.Fatalf("%d offset hook calls before the CQ, want 1", len(*hops))
	}
}

func TestPlainCQOnATrulyIdleBand(t *testing.T) {
	e, _ := newCQEnv(t, bus.ChaseRow{Call: normal, Band: "20m", Needed: true})
	_, reqs := e.run()
	cqs := cqOf(reqs)
	if len(cqs) != 1 || cqs[0].Directed {
		t.Fatalf("requests %q, want one plain CQ", describe(reqs))
	}
}

func TestASpotOnAnUnlistedBandLeavesTheBandIdle(t *testing.T) {
	e, _ := newCQEnv(t, expeditionRow("40m"))
	_, reqs := e.run()
	if cqs := cqOf(reqs); len(cqs) != 1 || cqs[0].Directed {
		t.Fatalf("requests %q, want one plain CQ", describe(reqs))
	}
}

func TestNoCQWhileANeededStationIsHeard(t *testing.T) {
	e, hops := newCQEnv(t, bus.ChaseRow{Call: normal, Band: "20m", Needed: true})
	// The needed station works another station, so auto7 cannot call it.
	_, reqs := e.run(other + " " + normal + " -10")
	if len(cqOf(reqs)) != 0 || len(*hops) != 0 {
		t.Fatalf("requests %q and %d hops while a needed station is heard, want none", describe(reqs), len(*hops))
	}
}

func TestCQRunEndsAndListenPeriodsFollow(t *testing.T) {
	e, hops := newCQEnv(t)
	if _, reqs := e.run(); len(cqOf(reqs)) != 1 {
		t.Fatalf("first period: requests %q, want a CQ", describe(reqs))
	}
	*hops = nil
	// WSJT-X sends a CQ in every second period, so ten CQ periods take
	// twenty periods. The run periods get no request and no hop.
	for i := 1; i < 20; i++ {
		task, reqs := e.run()
		if len(reqs) != 0 || task.Kind != TaskCQ || task.Requested {
			t.Fatalf("run period %d: task %+v, requests %q, want the running CQ and no request", i, task, describe(reqs))
		}
	}
	if len(*hops) != 0 {
		t.Fatalf("%d hops during the CQ run, want none", len(*hops))
	}
	// At least four listen periods follow. The band is idle for the
	// foraging, and the TX offset can move.
	listen := 0
	for {
		task, reqs := e.run()
		if len(cqOf(reqs)) > 0 {
			break
		}
		if task.Kind != TaskNone || task.Note != NoteCQListen {
			t.Fatalf("listen period %d: task %+v, want none with the listen note", listen+1, task)
		}
		listen++
		if listen > 10 {
			t.Fatal("no new CQ after ten listen periods")
		}
	}
	if listen < 4 {
		t.Fatalf("%d listen periods before the next CQ, want at least 4", listen)
	}
	if len(*hops) == 0 {
		t.Fatal("no offset hook call in the listen periods")
	}
}

func TestAnswerEndsTheCQRun(t *testing.T) {
	e, _ := newCQEnv(t)
	e.run()
	if _, reqs := e.run(ownCall + " " + caller + " AA11"); len(reqs) != 1 {
		t.Fatalf("requests %q, want the answer", describe(reqs))
	}
	// The answer replaced the CQ in WSJT-X. The next two periods belong
	// to the answer: the own TX period and the reply of the caller. They
	// get no request.
	for i := 1; i <= 2; i++ {
		if _, reqs := e.run(); len(reqs) != 0 {
			t.Fatalf("period %d after the answer: requests %q, want none", i, describe(reqs))
		}
	}
	// The caller did not reply. After the answer window, auto7 may call CQ
	// again.
	if _, reqs := e.run(); len(cqOf(reqs)) != 1 {
		t.Fatalf("requests %q after the answer window, want a new CQ", describe(reqs))
	}
}

func TestAcceptedAnswerSendsNoCQInTheSilentPeriod(t *testing.T) {
	e, hops := newCQEnv(t)
	spots := 0
	e.ch.SetSpotHook(func(context.Context, tx.State) bool { spots++; return false })
	task, reqs := e.run(ownCall + " " + caller + " AA11")
	if task.Kind != TaskAnswer || !task.Decision.OK || len(reqs) != 1 {
		t.Fatalf("task %+v, requests %q, want an accepted answer", task, describe(reqs))
	}
	*hops, spots = nil, 0
	// The own TX period of the answer has no decode, and the gate has
	// not opened the QSO view yet.
	task, reqs = e.run()
	if len(reqs) != 0 || len(*hops) != 0 || spots != 0 {
		t.Fatalf("silent period: requests %q, %d hops, %d spot checks, want none", describe(reqs), len(*hops), spots)
	}
	if task.Kind != TaskAnswer || task.Call != caller || task.Note != NoteContinue || task.Requested {
		t.Fatalf("silent period: task %+v, want the answer to %s with the continue note", task, caller)
	}
}

func TestOpenQSOEndsThePendingAnswer(t *testing.T) {
	e, _ := newCQEnv(t)
	e.run(ownCall + " " + caller + " AA11")
	e.gate.st.QSO = tx.QSOView{Open: true, DXCall: caller, Band: "20m"}
	if task, _ := e.run(ownCall + " " + caller + " R-05"); task.Kind != TaskQSO {
		t.Fatalf("task %+v, want QSO", task)
	}
	e.gate.st.QSO = tx.QSOView{}
	if _, reqs := e.run(); len(cqOf(reqs)) != 1 {
		t.Fatalf("requests %q after the QSO, want a CQ", describe(reqs))
	}
}

func TestStopEndsThePendingAnswer(t *testing.T) {
	e, _ := newCQEnv(t)
	e.run(ownCall + " " + caller + " AA11")
	e.gate.st.Arm = tx.Stopped
	if task, _ := e.run(); task.Note != NoteStopped {
		t.Fatalf("task %+v, want stopped", task)
	}
	e.gate.st.Arm = tx.Running
	if _, reqs := e.run(); len(cqOf(reqs)) != 1 {
		t.Fatalf("requests %q after a new enable, want a CQ", describe(reqs))
	}
}

func TestNoCQAndNoHopInReducedMode(t *testing.T) {
	e, hops := newCQEnv(t)
	e.gate.st.Build = wsjtx.BuildReduced
	task, reqs := e.run()
	if len(reqs) != 0 || len(*hops) != 0 {
		t.Fatalf("requests %q and %d hops with stock WSJT-X, want none", describe(reqs), len(*hops))
	}
	if task.Kind != TaskNone {
		t.Fatalf("task %+v, want none", task)
	}
}

func TestRefusedCQWaitsBeforeTheNextRequest(t *testing.T) {
	e, _ := newCQEnv(t)
	g := &refusingGate{fakeGate: e.gate}
	e.ch.gate = g
	e.run()
	if g.cqs != 1 {
		t.Fatalf("%d CQ requests, want 1", g.cqs)
	}
	for range 3 {
		e.run()
	}
	if g.cqs != 1 {
		t.Fatalf("%d CQ requests in the periods after a refusal, want 1", g.cqs)
	}
}

// refusingGate refuses each CQ request and counts them.
type refusingGate struct {
	*fakeGate
	cqs int
}

func (g *refusingGate) Submit(ctx context.Context, r tx.Request) tx.Decision {
	if _, ok := r.(tx.CQ); ok {
		g.cqs++
		return tx.Decision{Reason: tx.ReasonCQListen}
	}
	return g.fakeGate.Submit(ctx, r)
}
