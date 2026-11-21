// SPDX-License-Identifier: AGPL-3.0-or-later

package hunt

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/tx"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// Calls of the tests. All are invented.
const (
	ownCall = "XX9ZZ"
	normal  = "XX1AA" // needed, not high value
	atno    = "XY3CC" // needed, all-time new
	caller  = "XX2BB" // not needed, calls the operator
	other   = "XY4DD" // not needed
	normal2 = "XX5EE" // needed, not high value
)

// fakeGate records the requests and accepts each one.
type fakeGate struct {
	st   tx.State
	reqs []tx.Request
}

func (g *fakeGate) Submit(_ context.Context, r tx.Request) tx.Decision {
	g.reqs = append(g.reqs, r)
	return tx.Decision{OK: true}
}

func (g *fakeGate) State() tx.State { return g.st }

// take returns the requests since the last take.
func (g *fakeGate) take() []tx.Request {
	r := g.reqs
	g.reqs = nil
	return r
}

// env joins decodes with the needs and runs the task choice.
type env struct {
	t      *testing.T
	f      *fixture
	gate   *fakeGate
	ch     *Chooser
	period int
}

func newEnv(t *testing.T, foxDials ...uint64) *env {
	t.Helper()
	f := newFixture(t, foxDials...)
	f.needs.SetStatus(wsjtx.Status{DialHz: 14_074_000, Mode: "FT8", DECall: ownCall})
	f.needs.SetChase(chase(
		bus.ChaseRow{Call: normal, Band: "20m", Needed: true},
		bus.ChaseRow{Call: normal2, Band: "20m", Needed: true},
		bus.ChaseRow{Call: atno, Band: "20m", Needed: true,
			Needs: []bus.ChaseNeed{{Award: "dxcc", Key: "1", Status: "unworked", ATNO: true}}},
	))
	g := &fakeGate{st: tx.State{Arm: tx.Running, Build: wsjtx.BuildFull, Band: "20m", Mode: "FT8"}}
	limits := config.Limits{Tries: 4, TriesHighValue: 12, SkipMinutes: 10}
	return &env{t: t, f: f, gate: g, ch: NewChooser(g, f.needs.Lists(), limits, f.needs.OwnCall)}
}

// run feeds the decodes of one FT8 period and runs the choice after the
// last decode. It returns the task and the requests of the period.
func (e *env) run(texts ...string) (Task, []tx.Request) {
	e.t.Helper()
	e.period++
	e.f.clk.t = e.f.clk.t.Add(15 * time.Second)
	for _, text := range texts {
		d := decode(text)
		d.Time = 12*time.Hour + time.Duration(e.period)*15*time.Second
		s, ok := e.f.needs.Observe(d, e.f.clk.t)
		if !ok {
			e.t.Fatalf("decode %q not joined", text)
		}
		e.ch.Add(Heard{Station: s, Decode: d})
	}
	task := e.ch.Run(context.Background())
	return task, e.gate.take()
}

// describe gives a short text of each request for the test messages.
func describe(reqs []tx.Request) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		switch r := r.(type) {
		case tx.Call:
			out = append(out, "call "+r.Decode.Message)
		case tx.Answer:
			out = append(out, "answer "+r.Decode.Message)
		case tx.GiveUp:
			out = append(out, "give-up "+r.Call)
		case tx.Highlight:
			out = append(out, "highlight "+r.Call)
		default:
			out = append(out, fmt.Sprintf("%T", r))
		}
	}
	return out
}

func wantReqs(t *testing.T, step string, got []tx.Request, want ...string) {
	t.Helper()
	g := describe(got)
	if fmt.Sprint(g) != fmt.Sprint(want) {
		t.Fatalf("%s: requests = %q, want %q", step, g, want)
	}
}

func TestCallerInterruptsNormalHunt(t *testing.T) {
	e := newEnv(t)
	_, reqs := e.run("CQ " + normal + " AA00")
	wantReqs(t, "hunt starts", reqs, "call CQ "+normal+" AA00")

	task, reqs := e.run("CQ "+normal+" AA00", ownCall+" "+caller+" AA11")
	wantReqs(t, "caller", reqs, "answer "+ownCall+" "+caller+" AA11")
	if task.Kind != TaskAnswer || task.Call != caller {
		t.Fatalf("task = %+v, want answer %s", task, caller)
	}
}

func TestCallerWaitsForRunningQSO(t *testing.T) {
	e := newEnv(t)
	e.gate.st.QSO = tx.QSOView{Open: true, DXCall: normal, Band: "20m"}
	task, reqs := e.run(ownCall+" "+normal+" R-05", ownCall+" "+caller+" AA11")
	wantReqs(t, "QSO runs", reqs)
	if task.Kind != TaskQSO {
		t.Fatalf("task = %+v, want QSO", task)
	}

	e.gate.st.QSO = tx.QSOView{}
	_, reqs = e.run(ownCall + " " + caller + " AA11")
	wantReqs(t, "after the QSO", reqs, "answer "+ownCall+" "+caller+" AA11")
}

func TestCallerWaitsDuringHighValueHunt(t *testing.T) {
	e := newEnv(t)
	_, reqs := e.run("CQ " + atno + " AA00")
	wantReqs(t, "hunt starts", reqs, "call CQ "+atno+" AA00")

	task, reqs := e.run("CQ "+atno+" AA00", ownCall+" "+caller+" AA11")
	wantReqs(t, "caller during the hunt", reqs, "call CQ "+atno+" AA00")
	if task.Kind != TaskHighValue {
		t.Fatalf("task = %+v, want high-value hunt", task)
	}

	// The high-value station is not heard, and WSJT-X repeats the call.
	task, reqs = e.run(ownCall + " " + caller + " AA11")
	wantReqs(t, "caller while WSJT-X repeats", reqs)
	if task.Kind != TaskHighValue || task.Tries != 3 {
		t.Fatalf("task = %+v, want high-value hunt with 3 tries", task)
	}
}

func TestHighValueBeforeNormalStation(t *testing.T) {
	e := newEnv(t)
	_, reqs := e.run("CQ "+normal+" AA00", "CQ "+atno+" AA00")
	wantReqs(t, "both call CQ", reqs, "call CQ "+atno+" AA00")
}

func TestNoRequestWhileQSORuns(t *testing.T) {
	e := newEnv(t)
	e.gate.st.QSO = tx.QSOView{Open: true, DXCall: other, Band: "20m"}
	task, reqs := e.run("CQ "+atno+" AA00", "CQ "+normal+" AA00")
	wantReqs(t, "high value during a QSO", reqs)
	if task.Kind != TaskQSO || task.Call != other {
		t.Fatalf("task = %+v, want QSO with %s", task, other)
	}
}

func TestNoCallIntoOpenQSO(t *testing.T) {
	for _, text := range []string{
		other + " " + normal + " -10",
		other + " " + normal + " R-10",
		other + " " + normal + " AA00",
	} {
		e := newEnv(t)
		task, reqs := e.run(text)
		wantReqs(t, text, reqs)
		if task.Kind != TaskNone {
			t.Errorf("%s: task = %+v, want none", text, task)
		}
	}
}

func TestCallAfterQSOEnd(t *testing.T) {
	for _, end := range []string{"RR73", "RRR", "73"} {
		e := newEnv(t)
		_, reqs := e.run(other + " " + normal + " -10")
		wantReqs(t, "report", reqs)
		text := other + " " + normal + " " + end
		task, reqs := e.run(text)
		wantReqs(t, end, reqs, "call "+text)
		if task.Kind != TaskHunt || task.Call != normal || task.Tries != 1 {
			t.Errorf("%s: task = %+v, want hunt of %s with 1 try", end, task, normal)
		}
	}
}

func TestPauseWhenTargetWorksAnotherStation(t *testing.T) {
	e := newEnv(t)
	_, reqs := e.run("CQ " + normal + " AA00")
	wantReqs(t, "hunt starts", reqs, "call CQ "+normal+" AA00")

	_, reqs = e.run(other + " " + normal + " -10")
	wantReqs(t, "target answers another station", reqs, "give-up "+normal)
	if e.f.needs.Lists().Skipped(normal) {
		t.Fatal("a pause put the station on the skip list")
	}

	_, reqs = e.run(other + " " + normal + " R-03")
	wantReqs(t, "QSO of the target goes on", reqs)

	task, reqs := e.run(other + " " + normal + " RR73")
	wantReqs(t, "target ends its QSO", reqs, "call "+other+" "+normal+" RR73")
	if task.Tries != 2 {
		t.Fatalf("task = %+v, want 2 tries", task)
	}
}

func TestGiveUpAfterTries(t *testing.T) {
	e := newEnv(t)
	cq := "CQ " + normal + " AA00"
	for i := 1; i <= 4; i++ {
		task, reqs := e.run(cq)
		wantReqs(t, fmt.Sprintf("try %d", i), reqs, "call "+cq)
		if task.Tries != i {
			t.Fatalf("try %d: task = %+v", i, task)
		}
	}
	task, reqs := e.run(cq)
	wantReqs(t, "after 4 tries", reqs, "give-up "+normal)
	if fmt.Sprint(task.GaveUp) != fmt.Sprint([]string{normal}) {
		t.Fatalf("task = %+v, want %s given up", task, normal)
	}
	if !e.f.needs.Lists().Skipped(normal) {
		t.Fatal("station not on the skip list")
	}

	_, reqs = e.run(cq)
	wantReqs(t, "during the skip", reqs)

	e.f.clk.t = e.f.clk.t.Add(10 * time.Minute)
	task, reqs = e.run(cq)
	wantReqs(t, "after the skip", reqs, "call "+cq)
	if task.Tries != 1 {
		t.Fatalf("after the skip: task = %+v, want 1 try", task)
	}
}

func TestGiveUpGoesToNextStation(t *testing.T) {
	e := newEnv(t)
	cq := "CQ " + normal + " AA00"
	for i := 0; i < 4; i++ {
		e.run(cq)
	}
	cq2 := "CQ " + normal2 + " AA00"
	_, reqs := e.run(cq, cq2)
	wantReqs(t, "after 4 tries", reqs, "give-up "+normal, "call "+cq2)
}

func TestRepeatedCallCountsAsTry(t *testing.T) {
	e := newEnv(t)
	e.run("CQ " + normal + " AA00")
	for i := 2; i <= 4; i++ {
		task, reqs := e.run()
		wantReqs(t, "WSJT-X repeats", reqs)
		if task.Kind != TaskHunt || task.Tries != i {
			t.Fatalf("period %d: task = %+v, want hunt with %d tries", i, task, i)
		}
	}
	_, reqs := e.run()
	wantReqs(t, "after 4 tries", reqs, "give-up "+normal)
}

func TestHighValueGetsTwelveTries(t *testing.T) {
	e := newEnv(t)
	cq := "CQ " + atno + " AA00"
	for i := 1; i <= 12; i++ {
		_, reqs := e.run(cq)
		wantReqs(t, fmt.Sprintf("try %d", i), reqs, "call "+cq)
	}
	_, reqs := e.run(cq)
	wantReqs(t, "after 12 tries", reqs, "give-up "+atno)
}

func TestNoCQWhenNothingToDo(t *testing.T) {
	for _, build := range []wsjtx.Build{wsjtx.BuildReduced, wsjtx.BuildFull} {
		e := newEnv(t)
		e.gate.st.Build = build
		task, reqs := e.run("CQ "+other+" AA00", caller+" "+other+" -12")
		wantReqs(t, "idle band", reqs)
		if task.Kind != TaskNone || task.Note != NoteNoStation {
			t.Errorf("build %v: task = %+v, want no task", build, task)
		}
	}
}

func TestHighlightOnlyOnFoxHoundDial(t *testing.T) {
	e := newEnv(t, 14_090_000)
	e.f.needs.SetStatus(wsjtx.Status{DialHz: 14_090_000, Mode: "FT8", DECall: ownCall})
	_, reqs := e.run("CQ "+normal+" AA00", "CQ "+other+" AA00")
	wantReqs(t, "Fox/Hound dial", reqs, "highlight "+normal)

	_, reqs = e.run("CQ " + normal + " AA00")
	wantReqs(t, "second period", reqs)
}

func TestHighlightOnlyInHoundMode(t *testing.T) {
	e := newEnv(t)
	e.f.needs.SetStatus(wsjtx.Status{DialHz: 14_074_000, Mode: "FT8", DECall: ownCall, SpecialOp: wsjtx.SpecialOpHound})
	_, reqs := e.run("CQ "+atno+" AA00", ownCall+" "+caller+" AA11")
	wantReqs(t, "Hound mode", reqs, "highlight "+atno)
}

func TestHighlightColorsAreValid(t *testing.T) {
	e := newEnv(t, 14_090_000)
	e.f.needs.SetStatus(wsjtx.Status{DialHz: 14_090_000, Mode: "FT8", DECall: ownCall})
	_, reqs := e.run("CQ " + normal + " AA00")
	h, ok := reqs[0].(tx.Highlight)
	if !ok || !h.Bg.Valid || !h.Fg.Valid {
		t.Fatalf("request = %+v, want a highlight with valid colors", reqs[0])
	}
}

func TestCallCarriesDecodeAndReceiveTime(t *testing.T) {
	e := newEnv(t)
	_, reqs := e.run("CQ " + normal + " AA00")
	c, ok := reqs[0].(tx.Call)
	if !ok {
		t.Fatalf("request = %+v, want a call", reqs[0])
	}
	if c.Decode.Time != 12*time.Hour+15*time.Second || !c.Heard.Equal(e.f.clk.t) {
		t.Fatalf("call = %+v, want period 12:00:15 and heard %v", c, e.f.clk.t)
	}
}

func TestSpotHookBeforeCaller(t *testing.T) {
	e := newEnv(t)
	hooked := 0
	e.ch.SetSpotHook(func(context.Context, tx.State) bool { hooked++; return true })
	task, reqs := e.run(ownCall + " " + caller + " AA11")
	wantReqs(t, "spot", reqs)
	if task.Kind != TaskSpot || hooked != 1 {
		t.Fatalf("task = %+v, hook calls %d, want spot task and one call", task, hooked)
	}
}

func TestNoRequestWhileStopped(t *testing.T) {
	e := newEnv(t)
	e.gate.st.Arm = tx.Stopped
	task, reqs := e.run("CQ "+atno+" AA00", ownCall+" "+caller+" AA11")
	wantReqs(t, "stopped", reqs)
	if task.Note != NoteStopped {
		t.Fatalf("task = %+v, want stopped", task)
	}
}

func TestOldPeriodDecodesAreDropped(t *testing.T) {
	e := newEnv(t)
	s, _ := e.f.needs.Observe(decode("CQ "+normal+" AA00"), listNow)
	newer := decode("CQ " + normal2 + " AA00")
	newer.Time += 15 * time.Second
	s2, _ := e.f.needs.Observe(newer, listNow)
	e.ch.Add(Heard{Station: s, Decode: decode("CQ " + normal + " AA00")})
	e.ch.Add(Heard{Station: s2, Decode: newer})
	e.ch.Add(Heard{Station: s, Decode: decode("CQ " + normal + " AA00")})
	e.ch.Run(context.Background())
	wantReqs(t, "two periods", e.gate.take(), "call CQ "+normal2+" AA00")
}

func TestOwnCallFromStatus(t *testing.T) {
	f := newFixture(t)
	if got := f.needs.OwnCall(); got != "" {
		t.Fatalf("OwnCall before Status = %q", got)
	}
	f.needs.SetStatus(wsjtx.Status{DialHz: 14_074_000, Mode: "FT8", DECall: "xx9zz"})
	if got := f.needs.OwnCall(); got != ownCall {
		t.Fatalf("OwnCall = %q, want %q", got, ownCall)
	}
}
