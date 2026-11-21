// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"github.com/kgbvax/auto7/internal/bus"
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/wsjtx"
)

func TestArmState(t *testing.T) {
	if Stopped.Armed() || !Settling.Armed() || !Running.Armed() {
		t.Fatal("wrong Armed")
	}
	if Stopped.String() != "stopped" || Settling.String() != "settling" || Running.String() != "running" {
		t.Fatal("wrong names")
	}
}

func TestEnableTwiceSendsHoldOnce(t *testing.T) {
	h := newHarness(t).live().enable()
	h.enable()
	if n := countType(h.take(), wsjtx.TypeSetHoldTxFreq); n != 1 {
		t.Fatalf("%d SetHoldTxFreq, want 1", n)
	}
}

func TestStopWhileStoppedSendsNothing(t *testing.T) {
	h := newHarness(t).live()
	h.c.operatorStop(ReasonOperator)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("sent %v", c)
	}
	if s := h.c.snapshot(); s.StopReason != "" {
		t.Fatalf("reason %q, want none", s.StopReason)
	}
}

func TestOperatorStopLatches(t *testing.T) {
	h := newHarness(t).live().enable()
	h.c.operatorStop(ReasonOperator)
	h.sendStatus(nil)
	h.refresh()
	h.pass(30 * time.Second)
	h.wantStopped(ReasonOperator)
	h.enable()
	if s := h.c.snapshot(); s.StopReason != "" || s.NotStopped {
		t.Fatalf("state %+v after enable", s)
	}
}

// After the alarm that WSJT-X did not stop, a Stop by the operator or on
// the bus sends HaltTx again and starts a new confirm wait.
func TestStopAgainWhileNotConfirmedResendsHaltTx(t *testing.T) {
	for _, viaBus := range []bool{false, true} {
		h := newHarness(t).live().enable()
		h.status.Transmitting = true
		h.sendStatus(nil)
		h.c.operatorStop(ReasonOperator)
		h.pass(16 * time.Second) // no Status confirms the stop
		if s := h.c.snapshot(); !s.NotStopped {
			t.Fatalf("bus %v: state %+v, want the not stopped alarm", viaBus, s)
		}
		h.take()
		if viaBus {
			h.bus(bus.StopCommand{Meta: h.meta("site/hf/auto7/cmd"), Value: "stop"})
		} else {
			h.c.operatorStop(ReasonOperator)
		}
		if now, _ := halts(h.take()); now != 1 {
			t.Fatalf("bus %v: %d HaltTx at once after the second stop, want 1", viaBus, now)
		}
		h.wantStopped(ReasonOperator)
	}
}
