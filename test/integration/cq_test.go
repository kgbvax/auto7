// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/kgbvax/auto7/test/integration/peers"
)

// cqExpCall is an invented first-tier expedition. Its spot lies on the
// current band, and auto7 does not decode it.
const cqExpCall = "XY6FF"

// With the ws-hunter build and a band without a needed station or a
// caller, auto7 moves its TX offset to a clear place and calls a
// directed CQ. A high-value spot on a listed band makes the CQ directed.
func TestDirectedCQWithAnOffsetMove(t *testing.T) {
	e := newEnv(t)
	liveInputs(t, e)
	err := e.ChaseQueue.Set(map[string]any{
		"seq": 2, "activity": "dx",
		"ranked": []any{map[string]any{
			"row_id": 2, "call": cqExpCall, "band": "20m", "needed": true, "freq_hz": dialHz + 2200,
			"source": "cluster", "expedition": map[string]any{"call": cqExpCall, "tier": "first"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, e.WSJTX, baseStatus())
	p := startAuto7(t, e)
	p.enable(20 * time.Second)
	p.waitPage("<strong>Enabled</strong>", 5*time.Second)

	r.waitCount(peers.TypeSetTxEnabled, 1, 4*periodFT4)
	cmds := r.w.Commands()
	move, text, enable := -1, -1, -1
	for i, c := range cmds {
		switch {
		case c.Type == peers.TypeSetTxDF && move < 0:
			move = i
			if c.Number < 200 || c.Number > 4000 {
				t.Errorf("SetTxDF %d Hz, want 200 to 4000 Hz", c.Number)
			}
		case c.Type == peers.TypeFreeText && text < 0:
			text = i
			if want := "CQ DX " + ownCall + " " + ownGrid; c.Text != want || !c.Flag {
				t.Errorf("FreeText %q (send %v), want %q sent", c.Text, c.Flag, want)
			}
		case c.Type == peers.TypeSetTxEnabled && enable < 0:
			enable = i
			if !c.Flag {
				t.Error("SetTxEnabled false, want true")
			}
		case c.Type == peers.TypeClearDX:
			t.Error("ClearDX for a directed CQ")
		}
	}
	if move < 0 || text < 0 || enable < 0 || !(move < text && text < enable) {
		t.Fatalf("command order: SetTxDF at %d, FreeText at %d, SetTxEnabled at %d, want them in this order", move, text, enable)
	}
	if n := r.count(peers.TypeReply); n != 0 {
		t.Errorf("%d replies without a decode", n)
	}
}
