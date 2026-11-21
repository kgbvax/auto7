// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"strings"
	"time"

	"github.com/kgbvax/auto7/internal/wsjtx"
)

// RecordWindow is how long an own command stays in the record. A change
// in the WSJT-X Status that the record does not explain is a takeover by
// the operator. The window covers one FT8 period, because WSJT-X can
// report the effect of HaltTx only at the end of a transmission.
const RecordWindow = 20 * time.Second

// HintCallFirst is the hint after a DX call change that follows an own
// CQ with no own Reply. WSJT-X then answered a caller by itself.
const HintCallFirst = `The WSJT-X option "Call 1st" is likely on. Turn it off.`

// field names a Status field that auto7 commands.
type field int

const (
	fieldDial field = iota
	fieldMode
	fieldTxEnabled
	fieldTxDF
	fieldDXCall
)

func (f field) String() string {
	switch f {
	case fieldDial:
		return "dial"
	case fieldMode:
		return "mode"
	case fieldTxEnabled:
		return "Enable Tx"
	case fieldTxDF:
		return "TX offset"
	default:
		return "DX call"
	}
}

// expectation is one own command: a value that WSJT-X can report in
// Status until the time until.
type expectation struct {
	f     field
	num   uint64
	text  string
	until time.Time
}

// record holds the own commands of the last RecordWindow.
type record struct {
	entries []expectation
}

// add records that WSJT-X can report the value num or text in field f.
func (r *record) add(now time.Time, f field, num uint64, text string) {
	r.entries = append(r.entries, expectation{f: f, num: num, text: text, until: now.Add(RecordWindow)})
}

// has reports whether an own command explains the value num or text in
// field f at now. It drops the entries after their window.
func (r *record) has(now time.Time, f field, num uint64, text string) bool {
	kept := r.entries[:0]
	found := false
	for _, e := range r.entries {
		if now.After(e.until) {
			continue
		}
		kept = append(kept, e)
		if e.f == f && e.num == num && e.text == text {
			found = true
		}
	}
	clear(r.entries[len(kept):])
	r.entries = kept
	return found
}

func boolNum(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// sends73 reports whether a Tx message ends a QSO with 73 or RR73.
func sends73(msg string) bool {
	f := strings.Fields(strings.ToUpper(msg))
	if len(f) == 0 {
		return false
	}
	last := f[len(f)-1]
	return last == "73" || last == "RR73"
}

// takeover gives the first field that changed from prev to st with no
// own command in the record. ok is false when the record explains every
// change.
func (c *core) takeover(now time.Time, prev, st wsjtx.Status) (f field, ok bool) {
	if st.DialHz != prev.DialHz && !c.rec.has(now, fieldDial, st.DialHz, "") {
		return fieldDial, true
	}
	if mode := strings.ToUpper(st.Mode); mode != strings.ToUpper(prev.Mode) && !c.rec.has(now, fieldMode, 0, mode) {
		return fieldMode, true
	}
	if st.TxDF != prev.TxDF && !c.rec.has(now, fieldTxDF, uint64(st.TxDF), "") {
		return fieldTxDF, true
	}
	if st.TxEnabled != prev.TxEnabled && !c.rec.has(now, fieldTxEnabled, boolNum(st.TxEnabled), "") {
		// The Tx watchdog and the option "Disable Tx after sending 73"
		// turn Enable Tx off without the operator.
		auto := !st.TxEnabled && (st.TxWatchdog || sends73(st.TxMessage) || sends73(prev.TxMessage))
		if !auto {
			return fieldTxEnabled, true
		}
	}
	if dx := normCall(st.DXCall); dx != normCall(prev.DXCall) && !c.rec.has(now, fieldDXCall, 0, dx) {
		return fieldDXCall, true
	}
	return 0, false
}

// checkTakeover stops with ReasonTakeover when the Status shows a change
// that auto7 did not command.
func (c *core) checkTakeover(now time.Time, prev, st wsjtx.Status) {
	if !c.arm.Armed() {
		return
	}
	f, ok := c.takeover(now, prev, st)
	if !ok {
		return
	}
	hint := ""
	if f == fieldDXCall && c.cqOpen(now) {
		hint = HintCallFirst
	}
	c.log.Warn("tx: WSJT-X changed with no own command", "field", f.String())
	c.stop(now, ReasonTakeover)
	c.hint = hint
}

// cqOpen reports whether an own CQ runs or ended within its listen
// periods, with no own Reply since.
func (c *core) cqOpen(now time.Time) bool {
	return c.cq.open && (c.cq.active || now.Before(c.cq.listenUntil))
}
