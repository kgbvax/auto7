// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// call submits a call to XX1AA that WSJT-X decoded in the slot at t0, at
// offset df, heard 13 s into the slot. The clock must be in that slot.
func (h *harness) call(df uint32) Decision {
	return h.c.submit(Call{Decode: cqDecode(t0, df), Heard: t0.Add(13 * time.Second)})
}

// atCallTime sets the clock to 13.5 s into the slot at t0.
func (h *harness) atCallTime() { h.at(t0.Add(13500 * time.Millisecond)) }

// at moves the clock to t with pass and then refreshes the inputs.
func (h *harness) at(t time.Time) {
	h.pass(t.Sub(h.now))
	h.refresh()
}

func TestStartsStoppedAndSendsNothing(t *testing.T) {
	h := newHarness(t).live()
	h.pass(60 * time.Second)
	if s := h.c.snapshot(); s.Arm != Stopped {
		t.Fatalf("arm %v, want stopped", s.Arm)
	}
	h.at(t0.Add(73500 * time.Millisecond))
	d := h.c.submit(Call{Decode: cqDecode(t0.Add(60*time.Second), 800), Heard: t0.Add(73 * time.Second)})
	wantDecision(t, d, false, ReasonStopped)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("sent %v while stopped", c)
	}
}

// Presence lapses, the gate halts, refuses every request and
// stays stopped after presence returns.
func TestPresenceLapseStopsAndLatches(t *testing.T) {
	h := newHarness(t).live().enable()
	h.take()
	h.skip["presence"] = true
	h.pass(89 * time.Second)
	h.wantRunning()
	h.pass(2 * time.Second) // presence is now 91 s old
	h.wantStopped(ReasonNoPresence)
	now, auto := halts(h.take())
	if now != 1 || auto != 0 {
		t.Fatalf("HaltTx at once %d, auto only %d, want 1 and 0", now, auto)
	}
	h.skip["presence"] = false
	h.refresh()
	h.sendStatus(nil) // WSJT-X confirms the stop
	h.pass(60 * time.Second)
	h.wantStopped(ReasonNoPresence)
	// A call in the current slot is refused.
	slot := h.now.Truncate(15 * time.Second)
	d := h.c.submit(Call{Decode: cqDecode(slot, 800), Heard: h.now})
	wantDecision(t, d, false, ReasonStopped)
	d = h.c.submit(Answer{Decode: callerDecode(slot, 800), Heard: h.now})
	wantDecision(t, d, false, ReasonStopped)
	d = h.c.submit(CQ{Periods: 2})
	wantDecision(t, d, false, ReasonStopped)
	d = h.c.submit(OffsetMove{TxDF: 900})
	wantDecision(t, d, false, ReasonStopped)
	d = h.c.submit(Highlight{Call: "XX1AA"})
	wantDecision(t, d, false, ReasonStopped)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("sent %v after the stop", c)
	}
	// Only the operator leaves Stopped.
	h.enable()
}

// With the bus link: the link drops and presence lapses, both
// come back, and the gate stays stopped.
func TestBusLinkLostStopsAndHaltGoesOverUDP(t *testing.T) {
	h := newHarness(t).live().enable()
	h.take()
	h.bus(bus.Link{Up: false, Conn: 1, At: h.now, Reason: bus.BusDown})
	h.wantStopped(bus.BusDown)
	if now, _ := halts(h.take()); now != 1 {
		t.Fatalf("HaltTx at once %d, want 1", now)
	}
	h.skip["presence"] = true
	h.pass(100 * time.Second)
	h.bus(bus.Link{Up: true, Conn: 2, At: h.now})
	h.skip["presence"] = false
	h.refresh()
	h.pass(60 * time.Second)
	h.wantStopped(bus.BusDown)
	if r := countType(h.take(), wsjtx.TypeReply); r != 0 {
		t.Fatalf("%d Reply after the stop", r)
	}
}

func TestPresenceFromUnlistedHostDoesNotCount(t *testing.T) {
	h := newHarness(t)
	h.skip["presence"] = true
	h.live()
	h.bus(bus.Presence{Meta: h.meta("site/hf/logotter/presence"), Present: true, Host: "other-pc"})
	err := h.c.enable()
	if err == nil || err.Error() != ReasonNoPresence {
		t.Fatalf("enable: %v, want %q", err, ReasonNoPresence)
	}
	if s := h.c.snapshot(); s.Arm != Stopped || s.PresenceHost != "" {
		t.Fatalf("arm %v presence host %q, want stopped and no host", s.Arm, s.PresenceHost)
	}
	if c := h.take(); len(c) != 0 {
		t.Fatalf("sent %v", c)
	}
	// The laptop is listed, so its presence counts.
	h.bus(bus.Presence{Meta: h.meta("site/hf/logotter/presence"), Present: true, Host: laptopHost})
	h.enable()
	if s := h.c.snapshot(); s.PresenceHost != laptopHost {
		t.Fatalf("presence host %q, want %q", s.PresenceHost, laptopHost)
	}
}

func TestEnableWithoutPresenceIsRefusedWithTheReason(t *testing.T) {
	h := newHarness(t)
	h.skip["presence"] = true
	h.live()
	err := h.c.enable()
	if err == nil || err.Error() != "operator not present" {
		t.Fatalf("enable: %v, want operator not present", err)
	}
	if r := h.c.snapshot().LastRefusal; r.Kind != "enable" || r.Reason != "operator not present" {
		t.Fatalf("last refusal %+v", r)
	}
}

func TestHeartbeatStaleStops(t *testing.T) {
	h := newHarness(t).live().enable()
	h.take()
	h.skip["heartbeat"] = true
	h.pass(29 * time.Second)
	h.wantRunning()
	h.pass(2 * time.Second) // heartbeat 31 s old
	h.wantStopped("WSJT-X heartbeat stale")
	if now, _ := halts(h.take()); now != 1 {
		t.Fatalf("HaltTx at once %d, want 1", now)
	}
}

func TestChaseStaleRefusesCallAndAllowsAnswer(t *testing.T) {
	h := newHarness(t).live().enable()
	h.skip["chase"] = true
	h.pass(76 * time.Second)
	h.wantRunning()
	if !h.c.snapshot().ChaseStale {
		t.Fatal("chase not stale after 76 s")
	}
	h.take()
	slot := t0.Add(75 * time.Second)
	h.at(slot.Add(13500 * time.Millisecond))
	d := h.c.submit(Call{Decode: cqDecode(slot, 800), Heard: slot.Add(13 * time.Second)})
	wantDecision(t, d, false, "chase-queue state stale")
	d = h.c.submit(Answer{Decode: callerDecode(slot, 800), Heard: slot.Add(13 * time.Second)})
	wantDecision(t, d, true, "")
	c := h.take()
	if len(c) != 1 || c[0].Type != wsjtx.TypeReply || c[0].Text != ownCall+" XY3CC KK11" || c[0].Num != 800 {
		t.Fatalf("sent %v, want one Reply to XY3CC", c)
	}
}

func TestStopIsConfirmedOrRepeated(t *testing.T) {
	t.Run("no confirming Status", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.take()
		h.c.operatorStop(ReasonOperator)
		h.wantStopped(ReasonOperator)
		if now, _ := halts(h.take()); now != 1 {
			t.Fatalf("HaltTx at once %d, want 1", now)
		}
		// WSJT-X still transmits.
		h.sendStatus(func(s *wsjtx.Status) { s.TxEnabled, s.Transmitting = true, true })
		h.advance(14 * time.Second)
		if c := h.take(); len(c) != 0 || h.c.snapshot().NotStopped {
			t.Fatalf("sent %v before one period", c)
		}
		h.advance(2 * time.Second)
		if now, _ := halts(h.take()); now != 1 {
			t.Fatalf("second HaltTx at once %d, want 1", now)
		}
		s := h.c.snapshot()
		if !s.NotStopped {
			t.Fatal("state does not show that WSJT-X did not stop")
		}
		h.wantStopped(ReasonOperator)
		h.advance(30 * time.Second)
		if c := h.take(); len(c) != 0 {
			t.Fatalf("sent %v after the second HaltTx", c)
		}
		// A later confirming Status clears the flag.
		h.sendStatus(nil)
		if h.c.snapshot().NotStopped {
			t.Fatal("flag stays after a confirming Status")
		}
	})
	t.Run("confirming Status", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.c.operatorStop(ReasonOperator)
		h.take()
		h.advance(3 * time.Second)
		h.sendStatus(nil)
		h.advance(20 * time.Second)
		if c := h.take(); len(c) != 0 || h.c.snapshot().NotStopped {
			t.Fatalf("sent %v after a confirmed stop", c)
		}
	})
}

func TestRequestTiming(t *testing.T) {
	t.Run("in time", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.atCallTime()
		h.take()
		wantDecision(t, h.call(800), true, "")
		c := h.take()
		if len(c) != 1 || c[0].Type != wsjtx.TypeReply || c[0].Text != "CQ XX1AA KK11" {
			t.Fatalf("sent %v, want one Reply", c)
		}
	})
	t.Run("later than 2 s after the slot start", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.at(t0.Add(17500 * time.Millisecond))
		h.take()
		wantDecision(t, h.call(800), false, "too late for this slot")
		if c := h.take(); len(c) != 0 {
			t.Fatalf("sent %v", c)
		}
	})
	t.Run("just within 2 s after the slot start", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.at(t0.Add(16900 * time.Millisecond))
		wantDecision(t, h.call(800), true, "")
	})
	t.Run("decode older than one period", func(t *testing.T) {
		h := newHarness(t).live().enable()
		// The decode names the current slot, so only its age is wrong.
		slot := t0.Add(15 * time.Second)
		h.at(slot.Add(14 * time.Second))
		d := h.c.submit(Call{Decode: cqDecode(slot, 800), Heard: h.now.Add(-16 * time.Second)})
		wantDecision(t, d, false, "too late for this slot")
		d = h.c.submit(Answer{Decode: callerDecode(slot, 800), Heard: h.now.Add(-16 * time.Second)})
		wantDecision(t, d, false, "too late for this slot")
	})
	t.Run("FT4 slot", func(t *testing.T) {
		h := newHarness(t)
		h.status.Mode = "FT4"
		h.live().enable()
		// FT4 slots are 7.5 s long. The decode of the slot at t0 can go
		// out until 2 s after the next slot at t0+7.5 s.
		h.at(t0.Add(9 * time.Second))
		d := h.c.submit(Call{Decode: cqDecode(t0, 800), Heard: t0.Add(7 * time.Second)})
		wantDecision(t, d, true, "")
		h.c.operatorStop(ReasonOperator)
		h.enable()
		h.at(t0.Add(9600 * time.Millisecond))
		d = h.c.submit(Call{Decode: cqDecode(t0, 800), Heard: t0.Add(7 * time.Second)})
		wantDecision(t, d, false, ReasonTooLate)
	})
}

func TestEnableFullBuildSendsHoldTxFreq(t *testing.T) {
	h := newHarness(t).live()
	h.enable()
	c := h.take()
	if len(c) != 1 || c[0].Type != wsjtx.TypeSetHoldTxFreq || !c[0].Flag {
		t.Fatalf("sent %v, want SetHoldTxFreq true", c)
	}
	s := newHarness(t).stock().live()
	s.enable()
	if c := s.take(); len(c) != 0 {
		t.Fatalf("stock build: sent %v at Enable, want nothing", c)
	}
	if !s.c.snapshot().Reduced() || h.c.snapshot().Reduced() {
		t.Fatal("wrong reduced mode")
	}
}

func TestHoldTxFreqOffAfterReplyStops(t *testing.T) {
	for _, build := range []string{fullRev, stockRev} {
		t.Run(build, func(t *testing.T) {
			h := newHarness(t)
			h.build = build
			h.live().enable()
			h.atCallTime()
			wantDecision(t, h.call(800), true, "")
			h.take()
			// WSJT-X moved its TX offset to 20 Hz from the station.
			h.sendStatus(func(s *wsjtx.Status) { s.TxEnabled, s.DXCall, s.TxDF = true, "XX1AA", 820 })
			h.wantStopped("Hold Tx Freq off")
			if now, _ := halts(h.take()); now != 1 {
				t.Fatalf("HaltTx at once %d, want 1", now)
			}
		})
	}
}

func TestReplyWithHeldOffsetKeepsRunning(t *testing.T) {
	h := newHarness(t).live().enable()
	h.atCallTime()
	wantDecision(t, h.call(800), true, "")
	h.sendStatus(func(s *wsjtx.Status) { s.TxEnabled, s.DXCall = true, "XX1AA" })
	h.pass(20 * time.Second)
	h.wantRunning()
}

func TestCallNearOwnOffsetIsRefused(t *testing.T) {
	h := newHarness(t).stock().live().enable()
	h.atCallTime()
	h.take()
	wantDecision(t, h.call(1510), false, ReasonTooClose)
	d := h.c.submit(Answer{Decode: callerDecode(t0, 1490), Heard: t0.Add(13 * time.Second)})
	wantDecision(t, d, false, ReasonTooClose)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("sent %v", c)
	}
}

// The offset floor of 50 Hz holds even when the configuration asks for
// less.
func TestOffsetDistanceFloorIsHard(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Limits.OffsetMinDistanceHz = 10 }).live().enable()
	h.atCallTime()
	wantDecision(t, h.call(1530), false, ReasonTooClose)
	wantDecision(t, h.call(1550), true, "")
}

func TestEnableTxOffAfterReplyStops(t *testing.T) {
	h := newHarness(t).live().enable()
	h.atCallTime()
	wantDecision(t, h.call(800), true, "")
	h.take()
	h.sendStatus(func(s *wsjtx.Status) { s.DXCall = "XX1AA" })
	h.advance(10 * time.Second)
	h.sendStatus(func(s *wsjtx.Status) { s.DXCall = "XX1AA" })
	h.refresh()
	h.wantRunning()
	h.advance(6 * time.Second)
	h.wantStopped("automatic Tx enable off")
	if now, _ := halts(h.take()); now != 1 {
		t.Fatalf("HaltTx at once %d, want 1", now)
	}
}

func TestStockBuildRefusesCQOffsetAndBandChange(t *testing.T) {
	h := newHarness(t).stock().live().enable()
	h.take()
	wantDecision(t, h.c.submit(CQ{Directed: true, Periods: 2}), false, ReasonReduced)
	wantDecision(t, h.c.submit(CQ{Periods: 2}), false, ReasonReduced)
	wantDecision(t, h.c.submit(OffsetMove{TxDF: 900}), false, ReasonReduced)
	wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT8"}), false, ReasonReduced)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("sent %v", c)
	}
	if r := h.c.snapshot().LastRefusal; r.Kind != "band change" || r.Reason != ReasonReduced {
		t.Fatalf("last refusal %+v", r)
	}
}

func TestCQRunCeilingIsHard(t *testing.T) {
	// The configuration asks for more than the hard ceiling of 10.
	h := newHarness(t, func(c *config.Config) { c.Limits.CQMaxPeriods = 50 }).live().enable()
	h.take()
	wantDecision(t, h.c.submit(CQ{Periods: 11}), false, ReasonCQLimit)
	wantDecision(t, h.c.submit(CQ{Periods: 0}), false, ReasonCQLimit)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("sent %v", c)
	}
	wantDecision(t, h.c.submit(CQ{Periods: 10}), true, "")
	c := h.take()
	if len(c) != 2 || c[0].Type != wsjtx.TypeClearDX || c[1].Type != wsjtx.TypeSetTxEnabled || !c[1].Flag {
		t.Fatalf("plain CQ sent %v, want ClearDX and SetTxEnabled true", c)
	}
}

func TestCQRunLimitFromConfig(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Limits.CQMaxPeriods = 3 }).live().enable()
	wantDecision(t, h.c.submit(CQ{Periods: 4}), false, ReasonCQLimit)
}

func TestDirectedCQ(t *testing.T) {
	h := newHarness(t).live().enable()
	h.take()
	wantDecision(t, h.c.submit(CQ{Directed: true, Periods: 2}), true, "")
	c := h.take()
	if len(c) != 2 || c[0].Type != wsjtx.TypeFreeText || c[0].Text != "CQ DX XX0OP JJ00" || !c[0].Flag ||
		c[1].Type != wsjtx.TypeSetTxEnabled || !c[1].Flag {
		t.Fatalf("sent %v, want FreeText CQ DX and SetTxEnabled true", c)
	}
}

// transmit makes WSJT-X report one transmission of one period.
func (h *harness) transmit() {
	h.sendStatus(func(s *wsjtx.Status) { s.TxEnabled, s.Transmitting = true, true })
	h.pass(15 * time.Second)
	h.sendStatus(func(s *wsjtx.Status) { s.TxEnabled = true })
	h.pass(15 * time.Second)
}

func TestCQRunEndsAndListens(t *testing.T) {
	// The configuration asks for fewer listen periods than the floor of 4.
	h := newHarness(t, func(c *config.Config) { c.Limits.CQListenPeriods = 1 }).live().enable()
	wantDecision(t, h.c.submit(CQ{Periods: 2}), true, "")
	h.take()
	h.transmit()
	if _, auto := halts(h.take()); auto != 0 {
		t.Fatal("run ended after one of two periods")
	}
	h.sendStatus(func(s *wsjtx.Status) { s.TxEnabled, s.Transmitting = true, true })
	if _, auto := halts(h.take()); auto != 1 {
		t.Fatal("no HaltTx auto only at the last CQ period")
	}
	h.pass(15 * time.Second)
	h.sendStatus(nil)
	// The run ended. Four listen periods follow before a new run.
	h.pass(30 * time.Second)
	wantDecision(t, h.c.submit(CQ{Periods: 2}), false, ReasonCQListen)
	h.pass(31 * time.Second)
	wantDecision(t, h.c.submit(CQ{Periods: 2}), true, "")
	h.wantRunning()
}

func TestOffsetMoveCeilingIsHard(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Limits.OffsetMovesPerHour = 20 }).live().enable()
	h.take()
	for i := range 6 {
		wantDecision(t, h.c.submit(OffsetMove{TxDF: uint32(600 + 100*i)}), true, "")
		h.pass(time.Minute)
	}
	wantDecision(t, h.c.submit(OffsetMove{TxDF: 1900}), false, ReasonOffsetLimit)
	c := h.take()
	if n := countType(c, wsjtx.TypeSetTxDF); n != 6 {
		t.Fatalf("%d SetTxDF, want 6", n)
	}
	if c[0].Num != 600 {
		t.Fatalf("first SetTxDF %d Hz, want 600", c[0].Num)
	}
	// One hour after the first move, one move is free again.
	h.pass(55 * time.Minute)
	wantDecision(t, h.c.submit(OffsetMove{TxDF: 1900}), true, "")
}

func TestOffsetMoveRange(t *testing.T) {
	h := newHarness(t).live().enable()
	wantDecision(t, h.c.submit(OffsetMove{TxDF: 199}), false, ReasonOffsetRange)
	wantDecision(t, h.c.submit(OffsetMove{TxDF: 4001}), false, ReasonOffsetRange)
	wantDecision(t, h.c.submit(OffsetMove{TxDF: 200}), true, "")
}

func TestOffsetMovesFromConfig(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Limits.OffsetMovesPerHour = 0 }).live().enable()
	wantDecision(t, h.c.submit(OffsetMove{TxDF: 900}), false, ReasonOffsetLimit)
}

// peerState is the state of another auto7 host.
func (h *harness) peerState(host string, enabled, retained bool, ts time.Time) {
	m := h.meta("site/hf/auto7/" + host + "/state")
	m.Retained, m.TS = retained, ts
	h.bus(bus.PeerState{Meta: m, Host: host, Enabled: enabled})
}

func (h *harness) peerStatus(host, value string) {
	m := h.meta("site/hf/auto7/" + host + "/status")
	m.Retained = true
	h.bus(bus.PeerStatus{Meta: m, Host: host, Value: value})
}

func TestSecondControllerStops(t *testing.T) {
	h := newHarness(t).live().enable()
	h.take()
	h.peerStatus("op-laptop2", bus.StatusOnline)
	h.peerState("op-laptop2", true, false, h.now)
	h.wantStopped("second controller op-laptop2")
	if now, _ := halts(h.take()); now != 1 {
		t.Fatalf("HaltTx at once %d, want 1", now)
	}
	// Enable stays refused while the other host is enabled.
	if err := h.c.enable(); err == nil || !strings.HasPrefix(err.Error(), "second controller") {
		t.Fatalf("enable: %v, want second controller", err)
	}
}

func TestSecondControllerIgnoresOldOfflineAndOwn(t *testing.T) {
	h := newHarness(t).live().enable()
	h.peerStatus("op-laptop2", bus.StatusOnline)
	h.peerState("op-laptop2", true, true, h.now.Add(-91*time.Second))
	h.wantRunning()
	h.peerStatus("op-old", bus.StatusOffline)
	h.peerState("op-old", true, false, h.now)
	h.wantRunning()
	h.peerStatus(ownHost, bus.StatusOnline)
	h.peerState(ownHost, true, false, h.now)
	h.wantRunning()
	h.peerStatus("op-idle", bus.StatusOnline)
	h.peerState("op-idle", false, false, h.now)
	h.wantRunning()
}

// Two instances on one site, both enabled for more than 90 s. They did
// not see each other at first. When their retained states reach each
// other, the republish every 30 s has kept the ts fresh, so both stop.
func TestTwoInstancesBothStop(t *testing.T) {
	a := newHarness(t).live().enable()
	b := newHarness(t, func(c *config.Config) { c.Host = laptopHost })
	b.now = a.now
	b.live().enable()
	a.pass(100 * time.Second)
	b.pass(100 * time.Second)
	lastA, lastB := a.pubs[len(a.pubs)-1], b.pubs[len(b.pubs)-1]
	if !lastA.State.Enabled || !lastB.State.Enabled {
		t.Fatal("last published state is not enabled")
	}
	b.peerStatus(ownHost, bus.StatusOnline)
	b.peerState(ownHost, true, true, lastA.At)
	a.peerStatus(laptopHost, bus.StatusOnline)
	a.peerState(laptopHost, true, true, lastB.At)
	a.wantStopped("second controller " + laptopHost)
	b.wantStopped("second controller " + ownHost)
	if last := a.pubs[len(a.pubs)-1]; last.State.Enabled || last.State.StopReason != "second controller "+laptopHost {
		t.Fatalf("a published %+v after the stop", last.State)
	}
}

func TestGiveUp(t *testing.T) {
	t.Run("no new Reply in the period", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.atCallTime()
		wantDecision(t, h.call(800), true, "")
		h.sendStatus(func(s *wsjtx.Status) { s.TxEnabled, s.DXCall = true, "XX1AA" })
		h.take()
		wantDecision(t, h.c.submit(GiveUp{Call: "XX1AA"}), true, "")
		// Requests for the next slot can come until 2 s after its start.
		h.advance(3 * time.Second)
		if c := h.take(); len(c) != 0 {
			t.Fatalf("sent %v before the deadline", c)
		}
		h.advance(1 * time.Second)
		c := h.take()
		if now, auto := halts(c); now != 0 || auto != 1 || len(c) != 1 {
			t.Fatalf("sent %v, want one HaltTx auto only", c)
		}
		h.wantRunning()
	})
	t.Run("a new Reply in the period", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.atCallTime()
		wantDecision(t, h.call(800), true, "")
		h.sendStatus(func(s *wsjtx.Status) { s.TxEnabled, s.DXCall = true, "XX1AA" })
		wantDecision(t, h.c.submit(GiveUp{Call: "XX1AA"}), true, "")
		d := h.c.submit(Answer{Decode: callerDecode(t0, 900), Heard: t0.Add(13 * time.Second)})
		wantDecision(t, d, true, "")
		h.sendStatus(func(s *wsjtx.Status) { s.TxEnabled, s.DXCall = true, "XY3CC" })
		h.take()
		h.advance(10 * time.Second)
		if _, auto := halts(h.take()); auto != 0 {
			t.Fatal("HaltTx auto only after a new Reply")
		}
	})
}

func TestHighlightWhileRunning(t *testing.T) {
	h := newHarness(t).live().enable()
	h.take()
	wantDecision(t, h.c.submit(Highlight{Call: "XX1AA", Last: true}), true, "")
	if c := h.take(); len(c) != 1 || c[0].Type != wsjtx.TypeHighlightCall {
		t.Fatalf("sent %v, want HighlightCallsign", c)
	}
}

func TestBusStopCommandStops(t *testing.T) {
	h := newHarness(t).live().enable()
	h.take()
	h.bus(bus.StopCommand{Meta: h.meta("site/hf/auto7/cmd")})
	h.wantStopped(ReasonBusCommand)
	if now, _ := halts(h.take()); now != 1 {
		t.Fatal("no HaltTx at once")
	}
}

func TestPeerAddressChange(t *testing.T) {
	h := newHarness(t).live()
	other := wsjtx.Event{Kind: wsjtx.EventPeerChanged, Msg: wsjtx.Heartbeat{ID: wsjtxID, Revision: fullRev}, At: h.now}
	// While stopped, the gate lets the receiver learn the new address.
	h.c.handleWSJTX(other)
	if h.resets != 1 {
		t.Fatalf("%d peer resets, want 1", h.resets)
	}
	h.enable()
	h.take()
	h.c.handleWSJTX(other)
	h.wantStopped(ReasonPeerChanged)
	if now, _ := halts(h.take()); now != 1 {
		t.Fatal("no HaltTx at once")
	}
	if h.resets != 1 {
		t.Fatal("peer reset while armed")
	}
}

func TestPublishedStateCarriesTheTask(t *testing.T) {
	h := newHarness(t).live()
	h.task = "hunt XX1AA"
	h.enable()
	last := h.pubs[len(h.pubs)-1]
	if last.State.Task != "hunt XX1AA" {
		t.Fatalf("published task %q, want %q", last.State.Task, "hunt XX1AA")
	}
}

func TestPublishesStateAndRepublishes(t *testing.T) {
	h := newHarness(t).live()
	if n := len(h.pubs); n == 0 || h.pubs[n-1].State.Enabled {
		t.Fatalf("no disabled state at link up: %+v", h.pubs)
	}
	h.enable()
	last := h.pubs[len(h.pubs)-1]
	if !last.State.Enabled || last.State.Band != "20m" || last.State.Mode != "FT8" || last.State.Reduced != "" {
		t.Fatalf("published %+v after enable", last.State)
	}
	n := len(h.pubs)
	h.pass(61 * time.Second)
	if got := len(h.pubs) - n; got != 2 {
		t.Fatalf("%d republishes in 61 s, want 2", got)
	}
	h.c.operatorStop(ReasonOperator)
	last = h.pubs[len(h.pubs)-1]
	if last.State.Enabled || last.State.StopReason != ReasonOperator {
		t.Fatalf("published %+v after stop", last.State)
	}
	n = len(h.pubs)
	h.pass(61 * time.Second)
	if len(h.pubs) != n {
		t.Fatal("republished while stopped")
	}
	s := newHarness(t).stock().live()
	if r := s.pubs[len(s.pubs)-1].State.Reduced; r == "" {
		t.Fatal("stock build: no reduced mode in the state")
	}
}

func TestShutdownHalts(t *testing.T) {
	h := newHarness(t).live().enable()
	h.take()
	h.c.shutdown()
	h.wantStopped(ReasonExit)
	if now, _ := halts(h.take()); now != 1 {
		t.Fatal("no HaltTx at once at shutdown")
	}
	// A stopped and confirmed gate sends nothing at shutdown.
	s := newHarness(t).live()
	s.c.shutdown()
	if c := s.take(); len(c) != 0 {
		t.Fatalf("sent %v at shutdown while stopped", c)
	}
}

// A CQ in the slot of a give-up ends the give-up wait. The gate does not
// halt the CQ, and the CQ run ends after its periods.
func TestCQAfterGiveUpInTheSameSlot(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Limits.CQListenPeriods = 1 }).live().enable()
	h.atCallTime()
	h.take()
	wantDecision(t, h.c.submit(GiveUp{Call: "XX1AA"}), true, "")
	wantDecision(t, h.c.submit(CQ{Periods: 2}), true, "")
	h.take()
	// The give-up deadline is 2 s after the start of the next slot.
	h.advance(5 * time.Second)
	if now, auto := halts(h.take()); now != 0 || auto != 0 {
		t.Fatalf("HaltTx at once %d, auto only %d after the give-up deadline, want none", now, auto)
	}
	h.transmit()
	h.sendStatus(func(s *wsjtx.Status) { s.TxEnabled, s.Transmitting = true, true })
	if _, auto := halts(h.take()); auto != 1 {
		t.Fatal("no HaltTx auto only at the last CQ period")
	}
	h.pass(15 * time.Second)
	h.sendStatus(nil)
	h.pass(61 * time.Second)
	wantDecision(t, h.c.submit(CQ{Periods: 2}), true, "")
	h.wantRunning()
}

// A failed HaltTx at a stop keeps the wait for the confirming Status. The
// gate sends HaltTx again after one period.
func TestStopWithFailedHaltTx(t *testing.T) {
	h := newHarness(t).live().enable()
	h.take()
	h.conn.fail = failType(wsjtx.TypeHaltTx)
	h.c.operatorStop(ReasonOperator)
	h.wantStopped(ReasonOperator)
	if now, _ := halts(h.take()); now != 1 {
		t.Fatalf("HaltTx at once %d, want 1", now)
	}
	if !h.c.confirm.pending {
		t.Fatal("no wait for the confirming Status")
	}
	h.advance(16 * time.Second)
	if now, _ := halts(h.take()); now != 1 {
		t.Fatalf("second HaltTx at once %d, want 1", now)
	}
	if !h.c.confirm.pending || !h.c.snapshot().NotStopped {
		t.Fatal("state does not show that the stop has no confirmation")
	}
	h.wantStopped(ReasonOperator)
}

// Enable refuses when SetHoldTxFreq does not go out.
func TestEnableWithFailedHoldTxFreqIsRefused(t *testing.T) {
	h := newHarness(t).live()
	h.conn.fail = failType(wsjtx.TypeSetHoldTxFreq)
	err := h.c.enable()
	if err == nil || err.Error() != ReasonSendFailed {
		t.Fatalf("enable: %v, want %q", err, ReasonSendFailed)
	}
	if s := h.c.snapshot(); s.Arm != Stopped || s.LastRefusal.Reason != ReasonSendFailed {
		t.Fatalf("arm %v refusal %q, want stopped with %q", s.Arm, s.LastRefusal.Reason, ReasonSendFailed)
	}
}

// Enable refuses when the gate knows no WSJT-X address.
func TestEnableWithoutPeerAddressIsRefused(t *testing.T) {
	h := newHarness(t).live()
	h.noPeer = true
	err := h.c.enable()
	if err == nil || err.Error() != ReasonNoPeerAddr {
		t.Fatalf("enable: %v, want %q", err, ReasonNoPeerAddr)
	}
	if a := h.c.snapshot().Arm; a != Stopped {
		t.Fatalf("arm %v, want stopped", a)
	}
}

// A call before the first WSJT-X Status is refused.
func TestCallBeforeFirstStatusIsRefused(t *testing.T) {
	h := newHarness(t)
	h.bus(bus.Link{Up: true, Conn: h.link, At: h.now})
	h.refresh()
	h.enable()
	h.atCallTime()
	h.take()
	wantDecision(t, h.call(800), false, ReasonNoStatus)
	if n := countType(h.take(), wsjtx.TypeReply); n != 0 {
		t.Fatalf("%d Reply", n)
	}
}

// A directed CQ needs the own call and grid from WSJT-X.
func TestDirectedCQWithoutOwnCallOrGridIsRefused(t *testing.T) {
	for name, mod := range map[string]func(*wsjtx.Status){
		"no call": func(s *wsjtx.Status) { s.DECall = "" },
		"no grid": func(s *wsjtx.Status) { s.DEGrid = "" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			mod(&h.status)
			h.live().enable()
			h.take()
			wantDecision(t, h.c.submit(CQ{Directed: true, Periods: 2}), false, ReasonNoOwnCall)
			if c := h.take(); len(c) != 0 {
				t.Fatalf("sent %v", c)
			}
		})
	}
}

// A second CQ during an active CQ run is refused.
func TestSecondCQDuringRunIsRefused(t *testing.T) {
	h := newHarness(t).live().enable()
	wantDecision(t, h.c.submit(CQ{Periods: 2}), true, "")
	h.take()
	wantDecision(t, h.c.submit(CQ{Periods: 2}), false, ReasonCQActive)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("sent %v", c)
	}
}

// Without readable hunt lists, auto7 can call a station again that it
// just worked, so the gate refuses Enable.
func TestEnableIsRefusedWhileTheListsAreUnreadable(t *testing.T) {
	h := newHarness(t).live()
	h.listsErr = errors.New("lists unreadable: bad file")
	err := h.c.enable()
	if err == nil || err.Error() != ReasonListsUnreadable {
		t.Fatalf("enable = %v, want %q", err, ReasonListsUnreadable)
	}
	if s := h.c.snapshot(); s.Arm != Stopped {
		t.Fatalf("arm %v, want stopped", s.Arm)
	}
	h.listsErr = nil
	h.enable()
}
