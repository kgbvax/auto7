// SPDX-License-Identifier: AGPL-3.0-or-later

package hunt

import (
	"context"
	"slices"

	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/tx"
)

// TaskCQ means that auto7 calls CQ: the choice started a CQ run in this
// period, or a CQ run goes on.
const TaskCQ TaskKind = "CQ"

// Notes of the CQ task.
const (
	NoteCQDirected = "directed CQ"
	NoteCQPlain    = "plain CQ"
	NoteCQRuns     = "the CQ run goes on"
	// NoteCQListen marks the listen periods after a CQ run and the wait
	// after a refused CQ request. The band counts as idle for the band
	// foraging.
	NoteCQListen = "listen after the CQ run"
)

// cqExtraListen is the number of listen periods that the choice adds to
// the configured listen periods. The gate starts its listen periods at
// the last CQ transmission, and that can come one period after the
// choice expects it. With the extra periods, the next CQ request does not
// fall into the listen periods of the gate.
const cqExtraListen = 2

// OffsetHook may move the own TX offset. The task choice calls it only
// between tasks, never during a QSO or a CQ run. It reports whether the
// gate accepted a move. The band package fills it with Offsets.Hop. A nil
// hook moves nothing.
type OffsetHook func(ctx context.Context, st tx.State) bool

// cqRun is the own count of the task choice for the CQ run. The gate
// holds the hard limits. The count keeps the choice from requests that
// the gate refuses.
type cqRun struct {
	// run is the number of periods that the running CQ run still lasts.
	run int
	// listen is the number of periods to wait before the next CQ request.
	listen int
}

// SetOffsetHook sets the hook that moves the own TX offset.
func (c *Chooser) SetOffsetHook(h OffsetHook) {
	c.mu.Lock()
	c.offset = h
	c.mu.Unlock()
}

// SetSpotCheck sets the check for a high-value spot on a listed band.
// Such a spot makes the band not truly idle, so auto7 calls a directed CQ
// and no plain CQ. Use Needs.HighValueSpot. A nil check finds no spot.
func (c *Chooser) SetSpotCheck(f func() bool) {
	c.mu.Lock()
	c.spotCheck = f
	c.mu.Unlock()
}

// tickCQ counts one period of the CQ run and of the listen periods. Run
// calls it once per period before the choice. The caller holds mu.
func (c *Chooser) tickCQ() {
	switch {
	case c.cq.run > 0:
		c.cq.run--
		if c.cq.run == 0 {
			c.cq.listen = c.limits.CQListenPeriods + cqExtraListen
		}
	case c.cq.listen > 0:
		c.cq.listen--
	}
}

// afterTask ends the own count of a CQ run when another task replaced the
// CQ in WSJT-X: a QSO runs, or the gate accepted a call or an answer. The
// caller holds mu.
func (c *Chooser) afterTask(st tx.State, t Task) {
	if !st.Arm.Armed() {
		c.cq = cqRun{}
		return
	}
	if t.Kind == TaskQSO || (t.Kind != TaskCQ && t.Requested && t.Decision.OK) {
		c.cq.run = 0
	}
}

// chooseCQ is the last branch of the task choice: the band has no needed
// station and no caller. auto7 calls a directed CQ ("CQ DX"). Only on a
// truly idle band, where no listed band has a high-value spot, it calls a
// plain CQ. A spot that the band foraging does not follow, for example
// because it is "not heard here", still counts as a spot. With stock
// WSJT-X, auto7 calls no CQ and does not move its TX offset. The caller
// holds mu.
func (c *Chooser) chooseCQ(ctx context.Context, st tx.State, t Task) Task {
	if st.Arm != tx.Running || st.Reduced() || c.limits.CQMaxPeriods < 1 {
		t.Kind, t.Note = TaskNone, NoteNoStation
		return t
	}
	if c.cq.run > 0 {
		t.Kind, t.Note = TaskCQ, NoteCQRuns
		return t
	}
	// Between tasks: the TX offset can move.
	if c.offset != nil {
		c.offset(ctx, st)
	}
	if c.cq.listen > 0 || st.ChaseStale {
		t.Kind, t.Note = TaskNone, NoteCQListen
		return t
	}
	directed := c.spotCheck != nil && c.spotCheck()
	n := c.limits.CQMaxPeriods
	d := c.gate.Submit(ctx, tx.CQ{Directed: directed, Periods: n})
	t.Kind, t.Requested, t.Decision = TaskCQ, true, d
	t.Note = NoteCQPlain
	if directed {
		t.Note = NoteCQDirected
	}
	if !d.OK {
		// Wait the listen periods before the next request.
		c.cq.listen = c.limits.CQListenPeriods
		return t
	}
	// WSJT-X sends a CQ in every second period.
	c.cq.run = 2 * n
	return t
}

// HighValueSpot reports whether chase-queue lists a high-value station on
// a band of bands that auto7 did not work there. The band of a row comes
// from its frequency when it has one.
func (n *Needs) HighValueSpot(bands []config.Band) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for k, rows := range n.rows {
		for _, r := range rows {
			if !HighValue(r) {
				continue
			}
			band := k.band
			if r.FreqHz > 0 {
				if b := BandOf(uint64(r.FreqHz)); b != "" {
					band = b
				}
			}
			listed := slices.ContainsFunc(bands, func(b config.Band) bool { return b.Name == band })
			if listed && !n.lists.Worked(k.call, band) {
				return true
			}
		}
	}
	return false
}
