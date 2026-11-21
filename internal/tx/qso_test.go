// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/wsjtx"
)

// decode sends a new decode with the message msg from the current slot.
func (h *harness) decode(msg string) {
	d := cqDecode(slotStart(h.now, h.c.period()), 1200)
	d.Message = msg
	h.wsjtx(d)
}

// openQSO calls XX1AA, and XX1AA answers. It returns the receive time of
// the answer.
func (h *harness) openQSO() time.Time {
	h.t.Helper()
	h.callXX1AA()
	h.pass(14 * time.Second)
	h.decode(ownCall + " XX1AA -12")
	if !h.c.snapshot().QSO.Open {
		h.t.Fatal("QSO view not open after the answer")
	}
	return h.now
}

func TestQSOViewOpensOnAnswerOfDXCall(t *testing.T) {
	h := newHarness(t).live().enable()
	h.callXX1AA()
	// Another station calls the operator: not the DX call.
	h.decode(ownCall + " XY3CC KK11")
	// The DX call works another station.
	h.decode("XY2BB XX1AA -05")
	// A replay of an old decode does not count.
	old := cqDecode(slotStart(h.now, 15*time.Second), 1200)
	old.Message, old.New = ownCall+" XX1AA -12", false
	h.wsjtx(old)
	if q := h.c.snapshot().QSO; q.Open {
		t.Fatalf("QSO view %+v, want closed", q)
	}
	h.pass(14 * time.Second)
	h.decode(ownCall + " XX1AA -12")
	want := QSOView{Open: true, DXCall: "XX1AA", Started: h.now, Band: "20m"}
	if q := h.c.snapshot().QSO; q != want {
		t.Fatalf("QSO view %+v, want %+v", q, want)
	}
	// Later decodes of the partner keep the start time.
	h.pass(15 * time.Second)
	h.decode(ownCall + " XX1AA RR73")
	if q := h.c.snapshot().QSO; q != want {
		t.Fatalf("QSO view %+v, want %+v", q, want)
	}
}

func TestQSOViewEndsAfterFourPeriodsWithoutReply(t *testing.T) {
	t.Run("no reply", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.openQSO()
		h.pass(59 * time.Second)
		if !h.c.snapshot().QSO.Open {
			t.Fatal("QSO view closed before four periods")
		}
		h.pass(2 * time.Second)
		if q := h.c.snapshot().QSO; q.Open {
			t.Fatalf("QSO view %+v after four periods, want closed", q)
		}
		if w := h.c.snapshot().Warning; w != "" {
			t.Fatalf("warning %q, want none", w)
		}
	})
	t.Run("a reply restarts the count", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.openQSO()
		h.pass(30 * time.Second)
		h.decode(ownCall + " XX1AA RR73")
		h.pass(59 * time.Second)
		if !h.c.snapshot().QSO.Open {
			t.Fatal("QSO view closed before four periods after the reply")
		}
		h.pass(2 * time.Second)
		if h.c.snapshot().QSO.Open {
			t.Fatal("QSO view open four periods after the reply")
		}
	})
}

// sendEnd makes WSJT-X send msg to XX1AA for one transmission.
func (h *harness) sendEnd(msg string) {
	h.status.Transmitting, h.status.TxMessage = true, msg
	h.sendStatus(nil)
	h.pass(13 * time.Second)
	h.status.Transmitting = false
	h.sendStatus(nil)
}

func TestQSOViewEndsAfterSentRR73(t *testing.T) {
	t.Run("without QSOLogged", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.openQSO()
		h.pass(time.Second)
		sent := h.now
		h.sendEnd("XX1AA " + ownCall + " RR73")
		h.pass(sent.Add(29 * time.Second).Sub(h.now))
		if !h.c.snapshot().QSO.Open {
			t.Fatal("QSO view closed before two periods")
		}
		h.pass(2 * time.Second)
		s := h.c.snapshot()
		if s.QSO.Open {
			t.Fatalf("QSO view %+v two periods after RR73, want closed", s.QSO)
		}
		if s.Warning != "automatic log off" {
			t.Fatalf("warning %q, want automatic log off", s.Warning)
		}
		// The next QSOLogged clears the warning.
		h.wsjtx(wsjtx.QSOLogged{ID: wsjtxID, DXCall: "XY3CC"})
		if w := h.c.snapshot().Warning; w != "" {
			t.Fatalf("warning %q after QSOLogged", w)
		}
	})
	t.Run("with QSOLogged", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.openQSO()
		h.sendEnd("XX1AA " + ownCall + " RR73")
		h.wsjtx(wsjtx.QSOLogged{ID: wsjtxID, DXCall: "XX1AA"})
		s := h.c.snapshot()
		if s.QSO.Open || s.Warning != "" {
			t.Fatalf("QSO view %+v warning %q after QSOLogged, want closed and no warning", s.QSO, s.Warning)
		}
		h.pass(60 * time.Second)
		if w := h.c.snapshot().Warning; w != "" {
			t.Fatalf("warning %q", w)
		}
	})
	t.Run("sent 73", func(t *testing.T) {
		h := newHarness(t).live().enable()
		h.openQSO()
		h.pass(time.Second)
		sent := h.now
		h.sendEnd("XX1AA " + ownCall + " 73")
		h.pass(sent.Add(31 * time.Second).Sub(h.now))
		if h.c.snapshot().QSO.Open {
			t.Fatal("QSO view open two periods after 73")
		}
	})
}

// A running QSO is never stopped for another task.
func TestOpenQSORefusesOtherCalls(t *testing.T) {
	h := newHarness(t).live().enable()
	h.openQSO()
	h.take()
	slot := slotStart(h.now, 15*time.Second)
	d := h.c.submit(Answer{Decode: callerDecode(slot, 900), Heard: h.now})
	wantDecision(t, d, false, ReasonQSORuns)
	wantDecision(t, h.c.submit(CQ{Periods: 2}), false, ReasonQSORuns)
	if c := h.take(); len(c) != 0 {
		t.Fatalf("sent %v during the QSO", c)
	}
}
