// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// freshInputs gives inputs in which every source is live at t0.
func freshInputs() *inputs {
	in := newInputs("op-pc", []string{"op-pc"})
	m := func(topic string) bus.Meta { return bus.Meta{Topic: topic, Received: t0, TS: t0, Conn: 1} }
	in.handleBus(bus.Link{Up: true, Conn: 1, At: t0})
	in.handleBus(presenceMsg("op-pc", true, false, t0))
	in.handleBus(bus.ChaseState{Meta: m("site/hf/chase/state")})
	in.handleWSJTX(wsjtx.Event{Kind: wsjtx.EventPeerFixed, Msg: wsjtx.Heartbeat{ID: "WSJT-X", Revision: "r-hunter"}, At: t0})
	return in
}

func TestInputBlockers(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*inputs)
		at   time.Duration
		want string
	}{
		{"all fresh", nil, 15 * time.Second, ""},
		{"heartbeat 31 s old", nil, 31 * time.Second, "WSJT-X heartbeat stale"},
		{"no link", func(in *inputs) { in.handleBus(bus.Link{Up: false, Conn: 1, Reason: bus.BusDown}) }, 0, "bus down"},
		{"no heartbeat", func(in *inputs) { in.heartbeat = time.Time{} }, 0, "WSJT-X heartbeat stale"},
		{"no presence", func(in *inputs) { in.handleBus(presenceMsg("op-pc", false, false, t0)) }, 0, "operator not present"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := freshInputs()
			if tc.mod != nil {
				tc.mod(in)
			}
			if got := in.blocker(t0.Add(tc.at)); got != tc.want {
				t.Fatalf("blocker %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChaseFreshness(t *testing.T) {
	in := freshInputs()
	if !in.chaseFresh(t0.Add(75 * time.Second)) {
		t.Fatal("chase stale at 75 s")
	}
	if in.chaseFresh(t0.Add(76 * time.Second)) {
		t.Fatal("chase fresh at 76 s")
	}
	// A retained chase state does not make the state fresh.
	in.handleBus(bus.ChaseState{Meta: bus.Meta{Retained: true, Received: t0.Add(70 * time.Second)}})
	if in.chaseFresh(t0.Add(76 * time.Second)) {
		t.Fatal("retained chase state counted")
	}
}

func TestBuildFromHeartbeat(t *testing.T) {
	in := freshInputs()
	if in.build != wsjtx.BuildFull {
		t.Fatal("ws-hunter build not detected")
	}
	in.handleWSJTX(wsjtx.Event{Msg: wsjtx.Heartbeat{ID: "WSJT-X", Revision: "r"}, At: t0})
	if in.build != wsjtx.BuildReduced {
		t.Fatal("stock build not detected")
	}
}
