// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"strings"
	"time"

	"github.com/kgbvax/auto7/internal/wsjtx"
)

// WarnAutoLogOff is the warning when no QSOLogged follows an own RR73.
// The operator must turn on the automatic log in WSJT-X and turn off
// "Prompt me to log QSO".
const WarnAutoLogOff = "automatic log off"

// qsoTracker builds the QSO view only from WSJT-X data: Status, decodes
// with the own call, and QSOLogged.
type qsoTracker struct {
	view QSOView
	// lastHeard is the receive time of the last decode from the partner
	// that carried the own call.
	lastHeard time.Time
	// endSent is the time of the last Status that showed an own RR73 or
	// 73 to the partner on the air.
	endSent time.Time
	endRR73 bool
	warning string
}

// normCall gives a call in upper case without the brackets of a hashed
// call.
func normCall(s string) string {
	return strings.ToUpper(strings.Trim(strings.TrimSpace(s), "<>"))
}

// isGrid reports whether s is a four-character Maidenhead locator.
func isGrid(s string) bool {
	return len(s) == 4 && s[0] >= 'A' && s[0] <= 'R' && s[1] >= 'A' && s[1] <= 'R' &&
		s[2] >= '0' && s[2] <= '9' && s[3] >= '0' && s[3] <= '9'
}

// calls gives the addressee and the sender of a standard FT8 or FT4
// message. The addressee of a CQ is "CQ". Both are empty when the
// message has fewer than two words.
func calls(msg string) (to, from string) {
	f := strings.Fields(strings.ToUpper(msg))
	if len(f) < 2 {
		return "", ""
	}
	if f[0] != "CQ" {
		return normCall(f[0]), normCall(f[1])
	}
	// "CQ CALL", "CQ CALL GRID", "CQ DX CALL" or "CQ DX CALL GRID".
	if len(f) == 2 || (len(f) == 3 && isGrid(f[2])) {
		return "CQ", normCall(f[1])
	}
	return "CQ", normCall(f[2])
}

// decodeStart gives the start of the period of the decode d.
func decodeStart(now time.Time, d wsjtx.Decode) time.Time {
	start := midnight(now).Add(d.Time)
	if start.After(now.Add(time.Hour)) {
		// The decode is from the day before midnight.
		start = start.Add(-24 * time.Hour)
	}
	return start
}

// qsoDecode takes one decode that WSJT-X received at at. A QSO starts at
// the first decode from the DX call that carries the own call.
func (c *core) qsoDecode(at time.Time, d wsjtx.Decode) {
	if !d.New || d.OffAir || d.Time < 0 {
		return
	}
	if !c.bandAt.IsZero() && decodeStart(at, d).Before(c.bandAt) {
		// The gate drops every decode from before the band change.
		return
	}
	own := normCall(c.in.status.DECall)
	to, from := calls(d.Message)
	if own == "" || to != own || from == "" {
		return
	}
	q := &c.qso
	if q.view.Open {
		if from == q.view.DXCall {
			q.lastHeard = at
		}
		return
	}
	if from != normCall(c.in.status.DXCall) {
		return
	}
	q.view = QSOView{Open: true, DXCall: from, Started: at, Band: BandOf(c.in.status.DialHz)}
	q.lastHeard, q.endSent, q.endRR73 = at, time.Time{}, false
}

// qsoStatus notes an own RR73 or 73 to the partner on the air.
func (c *core) qsoStatus(now time.Time, st wsjtx.Status) {
	q := &c.qso
	if !q.view.Open || !st.Transmitting {
		return
	}
	to, _ := calls(st.TxMessage)
	if to != q.view.DXCall || !sends73(st.TxMessage) {
		return
	}
	f := strings.Fields(strings.ToUpper(st.TxMessage))
	q.endSent, q.endRR73 = now, f[len(f)-1] == "RR73"
}

// qsoLogged ends the QSO view at QSOLogged. WSJT-X logged the QSO, so
// the automatic log works.
func (c *core) qsoLogged() {
	c.qso = qsoTracker{}
}

// qsoTimers ends the QSO view two periods after an own RR73 or 73, or
// after four periods with no reply.
func (c *core) qsoTimers(now time.Time) {
	q := &c.qso
	if !q.view.Open {
		return
	}
	p := c.period()
	sentEnd := !q.endSent.IsZero() && !now.Before(q.endSent.Add(2*p))
	silent := !now.Before(q.lastHeard.Add(4 * p))
	if !sentEnd && !silent {
		return
	}
	if q.endRR73 {
		// WSJT-X logs at the own RR73 when the automatic log is on.
		q.warning = WarnAutoLogOff
	}
	q.view = QSOView{}
	q.lastHeard, q.endSent, q.endRR73 = time.Time{}, time.Time{}, false
}
