// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"strings"
	"time"

	"github.com/kgbvax/auto7/internal/wsjtx"
)

// Settle wait after a band change. auto7 reads no StationA state. It
// waits only for WSJT-X to report the new dial and mode.
const (
	// DialTimeout is the longest wait for a Status with the new dial and
	// mode after a band change. Then auto7 stops with ReasonNoDialChange.
	DialTimeout = 60 * time.Second
	// SettleTime is the least time after the first Status with the new
	// dial and mode before TX.
	SettleTime = 3 * time.Second
)

// settleStatus takes one Status during the settle wait. A Status with
// the expected dial, the expected mode and Transmitting false starts the
// settle time, if it has not started yet. Any other Status stops the
// settle time, so the next matching Status starts it again.
func (c *core) settleStatus(now time.Time, st wsjtx.Status) {
	b := c.band
	if c.arm != Settling || b == nil || b.phase != bandSettling {
		return
	}
	if st.DialHz == b.dial && strings.EqualFold(st.Mode, b.mode) && !st.Transmitting {
		if b.seen.IsZero() {
			b.seen = now
		}
		return
	}
	b.seen = time.Time{}
}

// txInBandChange reports whether st shows a transmission while the gate
// waits for the new dial and the settle time.
func (c *core) txInBandChange(st wsjtx.Status) bool {
	b := c.band
	return c.arm == Settling && b != nil && b.phase == bandSettling && st.Transmitting
}

// resumeSettle starts the settle wait again at Enable when a stop came
// after a dial change and before the settle wait ended. If the last
// Status already shows the new dial and mode, the settle time starts
// now. Else the gate waits for that Status up to DialTimeout.
func (c *core) resumeSettle(now time.Time) {
	b := c.band
	if b == nil || b.phase != bandSettling {
		c.band = nil
		return
	}
	b.sent, b.seen = now, time.Time{}
	st := c.in.status
	if c.in.hasStatus && st.DialHz == b.dial && strings.EqualFold(st.Mode, b.mode) && !st.Transmitting {
		b.seen = now
	}
}

// settled reports whether the settle wait is over. Without a band change
// it is over at once.
func (c *core) settled(now time.Time) bool {
	b := c.band
	if b == nil || b.phase != bandSettling {
		return true
	}
	return !b.seen.IsZero() && !now.Before(b.seen.Add(SettleTime))
}

// dialTimedOut reports whether DialTimeout passed after
// SetDialFrequency and no Status with the new dial and mode holds now.
func (c *core) dialTimedOut(now time.Time) bool {
	b := c.band
	return b != nil && b.phase == bandSettling && b.seen.IsZero() && !now.Before(b.sent.Add(DialTimeout))
}
