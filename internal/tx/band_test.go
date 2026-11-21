// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/wsjtx"
)

// The default FT8 and FT4 dials of WSJT-X, region 1, from
// models/FrequencyList.cpp. 160m has no FT4 dial.
func TestDefaultDials(t *testing.T) {
	want := map[string][2]uint64{
		"160m": {1_840_000, 0},
		"80m":  {3_573_000, 3_575_000},
		"40m":  {7_074_000, 7_047_500},
		"30m":  {10_136_000, 10_140_000},
		"20m":  {14_074_000, 14_080_000},
		"17m":  {18_100_000, 18_104_000},
		"15m":  {21_074_000, 21_140_000},
		"12m":  {24_915_000, 24_919_000},
		"10m":  {28_074_000, 28_180_000},
		"6m":   {50_313_000, 50_318_000},
	}
	for band, w := range want {
		for i, mode := range []string{"FT8", "FT4"} {
			hz, ok := DefaultDial(band, mode)
			if w[i] == 0 {
				if ok {
					t.Errorf("%s %s: dial %d, want none", band, mode, hz)
				}
				continue
			}
			if !ok || hz != w[i] {
				t.Errorf("%s %s: dial %d %v, want %d", band, mode, hz, ok, w[i])
			}
			if b := BandOf(hz); b != band {
				t.Errorf("band of %d Hz %q, want %q", hz, b, band)
			}
		}
	}
	for _, c := range []struct{ band, mode string }{{"60m", "FT8"}, {"2m", "FT8"}, {"20m", "JT65"}, {"", ""}} {
		if hz, ok := DefaultDial(c.band, c.mode); ok {
			t.Errorf("%s %s: dial %d, want none", c.band, c.mode, hz)
		}
	}
	if b := BandOf(5_357_000); b != "" {
		t.Errorf("band of 5357 kHz %q, want none", b)
	}
}

func TestBandChangeSendsDialThenConfigure(t *testing.T) {
	h := newHarness(t).live().enable()
	h.take()
	wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT4"}), true, "")
	if now, auto := halts(h.take()); now != 0 || auto != 1 {
		t.Fatal("no HaltTx auto only before the dial change")
	}
	var arms []ArmState
	h.conn.onWrite = func(cmd) { arms = append(arms, h.c.arm) }
	h.sendStatus(nil) // WSJT-X confirms the HaltTx.
	c := h.take()
	if len(c) != 2 || c[0].Type != wsjtx.TypeSetDialFrequency || c[0].Num != 18_104_000 ||
		c[1].Type != wsjtx.TypeConfigure || c[1].Text != "FT4" {
		t.Fatalf("sent %v, want SetDialFrequency 18104000 Hz and then Configure FT4", c)
	}
	for i, a := range arms {
		if a != Settling {
			t.Fatalf("arm %v when command %d went out, want settling", a, i)
		}
	}
	if s := h.c.snapshot(); s.Arm != Settling {
		t.Fatalf("arm %v after the band change, want settling", s.Arm)
	}
}

func TestBandChangeRefusals(t *testing.T) {
	t.Run("QSO view open", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.openQSO()
		h.take()
		wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT8"}), false, ReasonQSORuns)
		if c := h.take(); len(c) != 0 {
			t.Fatalf("sent %v", c)
		}
	})
	t.Run("WSJT-X transmits", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.callXX1AA()
		h.pass(2 * time.Second)
		h.status.Transmitting = true
		h.sendStatus(nil)
		h.take()
		wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT8"}), false, ReasonTransmits)
		if c := h.take(); len(c) != 0 {
			t.Fatalf("sent %v", c)
		}
	})
	t.Run("band or mode not in the band list", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.take()
		wantDecision(t, h.c.submit(BandChange{Band: "10m", Mode: "FT8"}), false, ReasonBandNotListed)
		wantDecision(t, h.c.submit(BandChange{Band: "40m", Mode: "FT4"}), false, ReasonBandNotListed)
		wantDecision(t, h.c.submit(BandChange{Band: "60m", Mode: "FT8"}), false, ReasonBandUnknown)
		if c := h.take(); len(c) != 0 {
			t.Fatalf("sent %v", c)
		}
		h.wantRunning()
	})
}

// WSJT-X keeps Enable Tx on across a dial change. So the gate halts
// first and waits for a Status that shows Enable Tx off.
// A Reply that WSJT-X has not reported yet can turn Enable Tx on after
// the last Status. The band change must still halt first and wait for a
// Status that comes after the HaltTx.
func TestBandChangeHaltsWhileOwnReplyIsInFlight(t *testing.T) {
	h := newHarness(t).live().enable()
	h.atCallTime()
	wantDecision(t, h.call(800), true, "")
	h.take()
	wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT8"}), true, "")
	c := h.take()
	if now, auto := halts(c); len(c) != 1 || now != 0 || auto != 1 {
		t.Fatalf("sent %v, want one HaltTx auto only", c)
	}
	h.advance(time.Second)
	if c := h.take(); countType(c, wsjtx.TypeSetDialFrequency) != 0 {
		t.Fatalf("sent %v before a Status after the HaltTx", c)
	}
	h.status.TxEnabled, h.status.DXCall = true, "XX1AA"
	h.sendStatus(nil) // The Reply arrives: Enable Tx is on.
	if c := h.take(); countType(c, wsjtx.TypeSetDialFrequency) != 0 {
		t.Fatalf("sent %v while Enable Tx is on", c)
	}
	h.status.TxEnabled = false
	h.sendStatus(nil)
	if c := h.take(); countType(c, wsjtx.TypeSetDialFrequency) != 1 {
		t.Fatalf("sent %v, want SetDialFrequency after Enable Tx off", c)
	}
}

func TestBandChangeHaltsFirst(t *testing.T) {
	h := newHarness(t).live().enable()
	h.callXX1AA()
	h.pass(3 * time.Second)
	h.take()
	wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT8"}), true, "")
	c := h.take()
	if now, auto := halts(c); len(c) != 1 || now != 0 || auto != 1 {
		t.Fatalf("sent %v, want one HaltTx auto only", c)
	}
	// Other requests wait for the band change.
	slot := slotStart(h.now, 15*time.Second)
	d := h.c.submit(Answer{Decode: callerDecode(slot, 900), Heard: h.now})
	wantDecision(t, d, false, ReasonBandChanging)
	wantDecision(t, h.c.submit(BandChange{Band: "20m", Mode: "FT4"}), false, ReasonBandChanging)
	h.sendStatus(nil) // Enable Tx is still on.
	h.pass(5 * time.Second)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("sent %v while Enable Tx is on", c)
	}
	h.wantRunning()
	h.status.TxEnabled = false
	h.sendStatus(nil)
	c = h.take()
	if len(c) != 2 || c[0].Type != wsjtx.TypeSetDialFrequency || c[0].Num != 18_100_000 ||
		c[1].Type != wsjtx.TypeConfigure || c[1].Text != "FT8" {
		t.Fatalf("sent %v, want SetDialFrequency and Configure", c)
	}
	if s := h.c.snapshot(); s.Arm != Settling {
		t.Fatalf("arm %v reason %q, want settling", s.Arm, s.StopReason)
	}
}

func TestBandChangeDroppedWhenWSJTXDoesNotHalt(t *testing.T) {
	h := newHarness(t).live().enable()
	h.callXX1AA()
	h.pass(3 * time.Second)
	h.take()
	wantDecision(t, h.c.submit(BandChange{Band: "17m", Mode: "FT8"}), true, "")
	h.take()
	h.pass(29 * time.Second)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("sent %v", c)
	}
	h.pass(2 * time.Second)
	if r := h.c.snapshot().LastRefusal; r.Kind != "band change" || r.Reason != ReasonNoHalt {
		t.Fatalf("last refusal %+v, want the band change with %q", r, ReasonNoHalt)
	}
	h.wantRunning()
	slot := slotStart(h.now, 15*time.Second)
	h.at(slot.Add(13500 * time.Millisecond))
	d := h.c.submit(Answer{Decode: callerDecode(slot, 900), Heard: slot.Add(13 * time.Second)})
	wantDecision(t, d, true, "")
}
