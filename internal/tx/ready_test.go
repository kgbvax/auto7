// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/wsjtx"
)

func (h *harness) wantSettling() {
	h.t.Helper()
	if s := h.c.snapshot(); s.Arm != Settling {
		h.t.Fatalf("arm %v reason %q, want settling", s.Arm, s.StopReason)
	}
}

// wantSettleRefusal fails unless the gate refuses an answer because it
// waits after a band change.
func (h *harness) wantSettleRefusal() {
	h.t.Helper()
	slot := slotStart(h.now, 15*time.Second)
	d := h.c.submit(Answer{Decode: callerDecode(slot, 900), Heard: h.now})
	wantDecision(h.t, d, false, ReasonSettling)
}

// bandChangeTo17m starts a band change from 20m to 17m FT8 on a gate
// that runs. WSJT-X has not reported the new dial yet.
func bandChangeTo17m(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t).live().enable()
	wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT8"}), true, "")
	h.sendStatus(nil) // WSJT-X confirms the HaltTx.
	h.wantSettling()
	h.take()
	return h
}

// A transmission during the settle wait means that Enable Tx came over
// to the new band. The gate stops at once.
func TestTransmitDuringSettleStops(t *testing.T) {
	h := bandChangeTo17m(t)
	h.status.DialHz = 18_100_000
	h.sendStatus(func(s *wsjtx.Status) { s.TxEnabled, s.Transmitting = true, true })
	h.wantStopped(ReasonTxInBandChange)
	if now, _ := halts(h.take()); now != 1 {
		t.Fatal("no HaltTx at once")
	}
}

// A stop in the middle of a band change keeps the new dial that auto7
// waits for. After Enable, the settle wait runs again.
func TestStopDuringSettleKeepsTheWait(t *testing.T) {
	h := bandChangeTo17m(t)
	h.c.operatorStop(ReasonOperator)
	h.status.TxEnabled, h.status.Transmitting = false, false
	h.sendStatus(nil)
	if err := h.c.enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	h.wantSettling()
	h.wantSettleRefusal()
	h.status.DialHz = 18_100_000
	h.sendStatus(nil)
	h.advance(2 * time.Second)
	h.wantSettling()
	h.advance(time.Second)
	h.wantRunning()
}

// The new dial can arrive while auto7 is stopped. After Enable, the
// settle time still runs in full.
func TestEnableAfterDialChangeWaitsSettleTime(t *testing.T) {
	h := bandChangeTo17m(t)
	h.c.operatorStop(ReasonOperator)
	h.status.DialHz = 18_100_000
	h.sendStatus(nil)
	h.advance(10 * time.Second)
	if err := h.c.enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	h.wantSettling()
	h.advance(2 * time.Second)
	h.wantSettling()
	h.advance(time.Second)
	h.wantRunning()
}

// After the change from 20m to 17m, auto7 does not transmit
// until a Status shows the 17m dial and mode, and 3 s passed after it.
func TestSettleWaitsForNewDialAndThreeSeconds(t *testing.T) {
	h := bandChangeTo17m(t)
	h.pass(5 * time.Second)
	h.wantSettling()
	h.wantSettleRefusal()
	h.status.DialHz = 18_100_000
	h.sendStatus(nil)
	seen := h.now
	h.advance(2900 * time.Millisecond)
	h.wantSettling()
	h.wantSettleRefusal()
	h.advance(seen.Add(3 * time.Second).Sub(h.now))
	h.wantRunning()
	if n := countType(h.take(), wsjtx.TypeReply); n != 0 {
		t.Fatalf("%d Reply while settling", n)
	}
}

// Only a Status with the expected dial and the expected mode counts.
func TestSettleNeedsModeAndReceive(t *testing.T) {
	h := newHarness(t).live().enable()
	wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT4"}), true, "")
	h.sendStatus(nil) // WSJT-X confirms the HaltTx.
	// The dial moved, but the mode is still FT8.
	h.status.DialHz = 18_104_000
	h.sendStatus(nil)
	h.advance(5 * time.Second)
	h.wantSettling()
	h.status.Mode = "FT4"
	h.sendStatus(nil)
	seen := h.now
	h.advance(2 * time.Second)
	h.wantSettling()
	h.advance(seen.Add(3 * time.Second).Sub(h.now))
	h.wantRunning()
}

func TestNoNewDialWithinSixtySecondsStops(t *testing.T) {
	h := bandChangeTo17m(t)
	// WSJT-X stays on the 20m dial.
	h.pass(59 * time.Second)
	h.wantSettling()
	h.pass(2 * time.Second)
	h.wantStopped("WSJT-X did not change band")
	if now, _ := halts(h.take()); now != 1 {
		t.Fatalf("HaltTx at once %d, want 1", now)
	}
}

// The band in the state comes from the WSJT-X dial.
func TestBandComesFromWSJTXDial(t *testing.T) {
	h := newHarness(t).live()
	if b := h.c.snapshot().Band; b != "20m" {
		t.Fatalf("band %q before the change, want 20m", b)
	}
	h.enable()
	wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT8"}), true, "")
	h.status.DialHz = 18_100_000
	h.sendStatus(nil)
	if b := h.c.snapshot().Band; b != "17m" {
		t.Fatalf("band %q after the new dial, want 17m", b)
	}
	if b := h.c.busState().Band; b != "17m" {
		t.Fatalf("published band %q, want 17m", b)
	}
}

// The gate drops every decode from before the band change.
func TestDecodeFromBeforeBandChangeIsRefused(t *testing.T) {
	h := newHarness(t).live().enable()
	h.at(t0.Add(12 * time.Second))
	wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT8"}), true, "")
	h.sendStatus(nil) // WSJT-X confirms the HaltTx.
	h.status.DialHz = 18_100_000
	h.sendStatus(nil)
	h.advance(3 * time.Second)
	h.wantRunning()
	// The decode of the slot at t0 arrives after the change.
	d := h.c.submit(Call{Decode: cqDecode(t0, 800), Heard: t0.Add(15 * time.Second)})
	wantDecision(t, d, false, ReasonOldDecode)
	if n := countType(h.take(), wsjtx.TypeReply); n != 0 {
		t.Fatalf("%d Reply", n)
	}
}

// A stop after the dial timeout does not keep the dial that WSJT-X did
// not reach. The next Enable runs on the current dial, and the forager
// can send a new band change.
func TestEnableAfterDialTimeoutRunsOnCurrentDial(t *testing.T) {
	h := bandChangeTo17m(t)
	bandAt := h.c.bandAt
	// WSJT-X stays on the 20m dial.
	h.pass(61 * time.Second)
	h.wantStopped(ReasonNoDialChange)
	h.sendStatus(nil) // WSJT-X confirms the stop.
	h.refresh()
	h.enable()
	if !h.c.bandAt.Equal(bandAt) {
		t.Fatalf("band change time %v after Enable, want %v", h.c.bandAt, bandAt)
	}
	wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT8"}), true, "")
}

// A dial change that did not go out is not expected. The next Enable
// runs on the current dial.
func TestEnableAfterDialSendFailureRunsOnCurrentDial(t *testing.T) {
	h := newHarness(t).live().enable()
	h.conn.fail = failType(wsjtx.TypeSetDialFrequency)
	wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT8"}), true, "")
	h.sendStatus(nil) // WSJT-X confirms the HaltTx.
	h.wantStopped(ReasonSendFailed)
	h.conn.fail = nil
	h.sendStatus(nil) // WSJT-X confirms the stop.
	h.enable()
}

// A failed Configure after the new dial went out also clears the dial
// that the gate waits for. The next Enable runs on the current dial.
func TestEnableAfterConfigureFailureRunsOnCurrentDial(t *testing.T) {
	h := newHarness(t).live().enable()
	h.conn.fail = failType(wsjtx.TypeConfigure)
	wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT4"}), true, "")
	h.sendStatus(nil) // WSJT-X confirms the HaltTx.
	h.wantStopped(ReasonSendFailed)
	h.conn.fail = nil
	h.sendStatus(nil) // WSJT-X confirms the stop.
	h.enable()
}
