// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

import (
	"fmt"
	"testing"
	"time"

	"github.com/kgbvax/auto7/test/integration/peers"
)

// Invented call and grid of the expedition in the band tests.
const (
	expCall = "XX2BB"
	expGrid = "JJ22"
)

// dial17FT4 is the WSJT-X default FT4 dial of 17 m.
const dial17FT4 = 18_104_000

// startAuto7Bands starts auto7 with 20 m and 17 m in the band list and
// with the fake horstreporter. It returns when the operator page answers.
func startAuto7Bands(t *testing.T, e *Env) *proc {
	t.Helper()
	return startAuto7With(t, e, fmt.Sprintf(`[horstreporter]
url = %q
qth = "JJ00"

[[bands]]
name = "20m"
modes = ["FT4"]

[[bands]]
name = "17m"
modes = ["FT4"]
`, e.Horstreporter.URL()))
}

// A first-tier expedition spot on 17 m moves auto7 from 20 m to 17 m.
// After SetDialFrequency, no Reply goes out until the fake WSJT-X reports
// the new dial and 3 s passed after that Status.
func TestBandChangeWaitsForTheNewDial(t *testing.T) {
	e := newEnv(t)
	liveInputs(t, e)
	err := e.Horstreporter.SetResponse("/api/dx_conditions", map[string]any{"bands": []any{
		map[string]any{"band": "20m", "activity_ratio": 1.0, "activity_level": "normal"},
		map[string]any{"band": "17m", "activity_ratio": 1.0, "activity_level": "normal"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = e.ChaseQueue.Set(map[string]any{
		"seq": 2, "activity": "dx",
		"ranked": []any{map[string]any{
			"row_id": 2, "call": expCall, "band": "17m", "needed": true, "freq_hz": dial17FT4 + 1200,
			"source": "cluster", "expedition": map[string]any{"call": expCall, "tier": "first"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, e.WSJTX, baseStatus())
	// WSJT-X keeps reporting the 20 m dial until the test releases it.
	r.mu.Lock()
	r.holdDial = true
	r.mu.Unlock()
	p := startAuto7Bands(t, e)
	p.enable(20 * time.Second)

	c, err := e.WSJTX.WaitCommand(peers.TypeSetDialFrequency, 3*periodFT4)
	if err != nil {
		t.Fatal(err)
	}
	if c.Number != dial17FT4 {
		t.Fatalf("SetDialFrequency %d, want %d", c.Number, dial17FT4)
	}
	if n := r.count(peers.TypeReply); n != 0 {
		t.Fatalf("%d replies before the band change", n)
	}
	waitSlot()
	p.waitPage("waiting for WSJT-X on the new band", time.Second)

	// WSJT-X reports the new dial 3.5 s into a slot. A decode of that
	// slot comes at once, inside the 3 s after the Status.
	slot := slotStart(time.Now())
	time.Sleep(time.Until(slot.Add(3500 * time.Millisecond)))
	dialAt := r.releaseDial()
	r.decode("CQ "+expCall+" "+expGrid, 1200)
	time.Sleep(1500 * time.Millisecond)
	if n := r.count(peers.TypeReply); n != 0 {
		t.Fatalf("%d replies within 3 s after the Status with the new dial", n)
	}
	p.waitPage("waiting for WSJT-X on the new band", time.Second)

	// The settle time passed: auto7 calls the expedition on 17 m.
	waitSlot()
	r.decode("CQ "+expCall+" "+expGrid, 1200)
	r.waitCount(peers.TypeReply, 1, 3*time.Second)
	for _, c := range r.w.Commands() {
		if c.Type != peers.TypeReply {
			continue
		}
		if c.Text != "CQ "+expCall+" "+expGrid {
			t.Errorf("Reply to %q, want the expedition", c.Text)
		}
		if c.At.Before(dialAt.Add(3 * time.Second)) {
			t.Errorf("Reply %v after the Status with the new dial, want at least 3 s", c.At.Sub(dialAt))
		}
	}
}
