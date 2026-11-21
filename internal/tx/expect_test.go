// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"strings"
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/wsjtx"
)

// callXX1AA calls XX1AA and lets WSJT-X report the Reply: Enable Tx on
// and the DX call set. The new values stay in the Status of the harness.
func (h *harness) callXX1AA() {
	h.t.Helper()
	h.atCallTime()
	wantDecision(h.t, h.call(800), true, "")
	h.status.TxEnabled, h.status.DXCall = true, "XX1AA"
	h.sendStatus(nil)
	h.wantRunning()
}

// The operator presses Halt Tx in WSJT-X: Enable Tx goes off
// with no own command in the record.
func TestOperatorEnableTxOffIsTakeover(t *testing.T) {
	h := newHarness(t).live().enable()
	h.callXX1AA()
	h.pass(RecordWindow + 5*time.Second)
	h.wantRunning()
	h.take()
	h.status.TxEnabled = false
	h.sendStatus(nil)
	h.wantStopped("operator took over")
	if now, _ := halts(h.take()); now != 1 {
		t.Fatalf("HaltTx at once %d, want 1", now)
	}
	if hint := h.c.snapshot().Hint; hint != "" {
		t.Fatalf("hint %q, want none", hint)
	}
	// auto7 does not call in the next cycle.
	slot := slotStart(h.now, 15*time.Second).Add(15 * time.Second)
	h.at(slot.Add(13500 * time.Millisecond))
	d := h.c.submit(Call{Decode: cqDecode(slot, 800), Heard: slot.Add(13 * time.Second)})
	wantDecision(t, d, false, ReasonStopped)
	if r := countType(h.take(), wsjtx.TypeReply); r != 0 {
		t.Fatalf("%d Reply after the takeover", r)
	}
}

func TestOperatorChangesAreTakeovers(t *testing.T) {
	cases := []struct {
		name   string
		change func(*wsjtx.Status)
	}{
		{"dial that auto7 did not command", func(s *wsjtx.Status) { s.DialHz = 14_080_000 }},
		{"mode change by the operator", func(s *wsjtx.Status) { s.Mode = "FT4" }},
		{"DX call change from a double-click", func(s *wsjtx.Status) { s.DXCall = "XY3CC" }},
		{"Enable Tx on by the operator", func(s *wsjtx.Status) { s.TxEnabled = true }},
		{"TX offset that auto7 did not command", func(s *wsjtx.Status) { s.TxDF = 1700 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t).live().enable()
			h.pass(5 * time.Second)
			h.take()
			tc.change(&h.status)
			h.sendStatus(nil)
			h.wantStopped(ReasonTakeover)
			if now, _ := halts(h.take()); now != 1 {
				t.Fatalf("HaltTx at once %d, want 1", now)
			}
			if hint := h.c.snapshot().Hint; hint != "" {
				t.Fatalf("hint %q, want none without an own CQ", hint)
			}
		})
	}
}

func TestOwnOffsetMoveIsNotTakeover(t *testing.T) {
	h := newHarness(t).live().enable()
	wantDecision(t, h.c.submit(OffsetMove{TxDF: 900}), true, "")
	h.pass(time.Second)
	h.status.TxDF = 900
	h.sendStatus(nil)
	h.wantRunning()
	h.pass(5 * time.Second)
	h.status.TxDF = 1000
	h.sendStatus(nil)
	h.wantStopped(ReasonTakeover)
}

func TestDialRecordWindow(t *testing.T) {
	t.Run("inside the window", func(t *testing.T) {
		h := newHarness(t).live().enable()
		wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT8"}), true, "")
		h.sendStatus(nil) // WSJT-X confirms the HaltTx.
		h.pass(5 * time.Second)
		h.status.DialHz = 18_100_000
		h.sendStatus(nil)
		if s := h.c.snapshot(); s.Arm != Settling {
			t.Fatalf("arm %v reason %q, want settling", s.Arm, s.StopReason)
		}
	})
	t.Run("after the window", func(t *testing.T) {
		h := newHarness(t).live().enable()
		wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT8"}), true, "")
		h.sendStatus(nil) // WSJT-X confirms the HaltTx.
		h.pass(RecordWindow + time.Second)
		h.status.DialHz = 18_100_000
		h.sendStatus(nil)
		h.wantStopped(ReasonTakeover)
	})
}

// Configure FT8 to FT4 can move the dial. The record holds the FT4 dial
// and the mode, so the Status after the change is no takeover.
func TestConfigureMovesDialInsideWindow(t *testing.T) {
	h := newHarness(t).live().enable()
	wantDecision(t, h.c.submit(BandChange{Band: "20m", Mode: "FT4"}), true, "")
	h.sendStatus(nil) // WSJT-X confirms the HaltTx.
	h.pass(2 * time.Second)
	h.status.Mode = "FT4"
	h.sendStatus(nil)
	h.pass(time.Second)
	h.status.DialHz = 14_080_000
	h.sendStatus(nil)
	if s := h.c.snapshot(); s.Arm != Settling {
		t.Fatalf("arm %v reason %q, want settling", s.Arm, s.StopReason)
	}
}

func TestAutomaticEnableTxOffIsNotTakeover(t *testing.T) {
	cases := []struct {
		name   string
		change func(*wsjtx.Status)
	}{
		{"after 73", func(s *wsjtx.Status) { s.TxMessage = "XX1AA XX0OP 73" }},
		{"after RR73", func(s *wsjtx.Status) { s.TxMessage = "XX1AA XX0OP RR73" }},
		{"Tx watchdog", func(s *wsjtx.Status) { s.TxWatchdog = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t).live().enable()
			h.callXX1AA()
			h.pass(RecordWindow + 5*time.Second)
			h.status.TxEnabled = false
			tc.change(&h.status)
			h.sendStatus(nil)
			h.wantRunning()
		})
	}
	t.Run("another message", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.callXX1AA()
		h.pass(RecordWindow + 5*time.Second)
		h.status.TxEnabled, h.status.TxMessage = false, "XX1AA XX0OP R-10"
		h.sendStatus(nil)
		h.wantStopped(ReasonTakeover)
	})
}

// A DX call change right after an own CQ and with no own Reply comes
// from WSJT-X itself when its option "Call 1st" is on.
func TestDXCallChangeAfterOwnCQ(t *testing.T) {
	h := newHarness(t).live().enable()
	wantDecision(t, h.c.submit(CQ{Periods: 2}), true, "")
	h.status.TxEnabled, h.status.Transmitting = true, true
	h.sendStatus(nil)
	h.pass(13 * time.Second)
	h.status.Transmitting = false
	h.sendStatus(nil)
	h.pass(10 * time.Second)
	h.wantRunning()
	h.status.DXCall = "XY3CC"
	h.sendStatus(nil)
	h.wantStopped(ReasonTakeover)
	hint := h.c.snapshot().Hint
	if hint != HintCallFirst || !strings.Contains(hint, `"Call 1st"`) {
		t.Fatalf("hint %q, want one that names the option Call 1st", hint)
	}
	// Enable clears the hint.
	h.sendStatus(nil)
	h.enable()
	if hint := h.c.snapshot().Hint; hint != "" {
		t.Fatalf("hint %q after enable", hint)
	}
}

// WSJT-X can clear the DX call after it logged the QSO.
func TestDXCallClearedAfterQSOLoggedIsNotTakeover(t *testing.T) {
	h := newHarness(t).live().enable()
	h.callXX1AA()
	h.pass(RecordWindow + 5*time.Second)
	h.wsjtx(wsjtx.QSOLogged{ID: wsjtxID, DXCall: "XX1AA"})
	h.pass(time.Second)
	h.status.DXCall = ""
	h.sendStatus(nil)
	h.wantRunning()
}

func TestWSJTXRestartStops(t *testing.T) {
	t.Run("Close", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.take()
		h.wsjtx(wsjtx.Close{ID: wsjtxID})
		h.wantStopped("WSJT-X restarted")
		if now, _ := halts(h.take()); now != 1 {
			t.Fatalf("HaltTx at once %d, want 1", now)
		}
	})
	t.Run("new revision", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.pass(10 * time.Second)
		h.wantRunning()
		h.build = "def456-hunter"
		h.refresh()
		h.wantStopped(ReasonRestarted)
	})
	t.Run("new version", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.wsjtx(wsjtx.Heartbeat{ID: wsjtxID, MaxSchema: 3, Version: "2.7.1", Revision: fullRev})
		h.wantStopped(ReasonRestarted)
	})
	t.Run("a change while stopped does not block Enable", func(t *testing.T) {
		h := newHarness(t).live()
		h.build = "def456-hunter"
		h.refresh()
		h.enable()
		h.pass(30 * time.Second)
		h.wantRunning()
	})
}

// Covers Special Op Mode 7 (Hound): no call, no answer and no CQ. A
// highlight still goes out.
func TestSpecialOpModeRefusesCalls(t *testing.T) {
	h := newHarness(t)
	h.status.SpecialOp = wsjtx.SpecialOpHound
	h.live().enable()
	h.atCallTime()
	h.take()
	wantDecision(t, h.call(800), false, ReasonSpecialOp)
	d := h.c.submit(Answer{Decode: callerDecode(t0, 900), Heard: t0.Add(13 * time.Second)})
	wantDecision(t, d, false, ReasonSpecialOp)
	wantDecision(t, h.c.submit(CQ{Periods: 2}), false, ReasonSpecialOp)
	wantDecision(t, h.c.submit(Highlight{Call: "XX1AA"}), true, "")
	c := h.take()
	if len(c) != 1 || c[0].Type != wsjtx.TypeHighlightCall {
		t.Fatalf("sent %v, want one HighlightCallsign", c)
	}
	h.wantRunning()
}
