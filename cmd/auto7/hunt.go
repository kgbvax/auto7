// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kgbvax/auto7/internal/bands"
	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/hunt"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// Timing of the task choice.
const (
	// settleAfterDecode is the wait after the last decode of a period.
	// Then the task choice runs for that period.
	settleAfterDecode = 500 * time.Millisecond
	// silentOffset is the time into a slot at which the task choice runs
	// for the period before, when that period gave no decode.
	silentOffset = time.Second
)

// Periods of the modes.
const (
	periodFT8 = 15 * time.Second
	periodFT4 = 7500 * time.Millisecond
)

// periodOf gives the T/R period of a WSJT-X Status. It gives the FT8
// period when the Status names no known period.
func periodOf(st wsjtx.Status) time.Duration {
	switch {
	case st.Mode == config.ModeFT4:
		return periodFT4
	case st.Mode == config.ModeFT8:
		return periodFT8
	case st.TRPeriod != wsjtx.NotApplicable && st.TRPeriod > 0:
		return time.Duration(st.TRPeriod) * time.Second
	}
	return periodFT8
}

// midnight gives the start of the UTC day of t.
func midnight(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// slotStart gives the start of the slot that holds now.
func slotStart(now time.Time, p time.Duration) time.Time {
	day := midnight(now)
	off := now.Sub(day)
	return day.Add(off - off%p)
}

// decodeStart gives the start of the period of a decode that arrived at
// at. since is the time of the period since midnight UTC.
func decodeStart(at time.Time, since time.Duration) time.Time {
	start := midnight(at).Add(since)
	if start.After(at.Add(time.Hour)) {
		// The decode is from the day before midnight.
		start = start.Add(-24 * time.Hour)
	}
	return start
}

// slots decides when the task choice runs: once per period, shortly after
// the last decode of the period. A period with no decode gets its run at
// silentOffset into the next slot, so a running hunt still counts its
// tries.
type slots struct {
	period time.Duration
	// pending is the start of the period whose decodes wait for the run.
	// It is zero when no decode waits.
	pending time.Time
	// last is the receive time of the last decode of pending.
	last time.Time
	// done is the start of the last period that had its run.
	done time.Time
}

// decode notes a decode of the period that starts at start and that
// arrived at at. It reports whether the task choice takes the decode. It
// does not take a decode of a period that already had its run.
func (s *slots) decode(start, at time.Time) bool {
	if !start.After(s.done) || (!s.pending.IsZero() && start.Before(s.pending)) {
		return false
	}
	s.pending, s.last = start, at
	return true
}

// due gives the period whose run is due at now.
func (s *slots) due(now time.Time) (time.Time, bool) {
	if !s.pending.IsZero() {
		return s.pending, !now.Before(s.last.Add(settleAfterDecode))
	}
	cur := slotStart(now, s.period)
	prev := cur.Add(-s.period)
	if !now.Before(cur.Add(silentOffset)) && prev.After(s.done) {
		return prev, true
	}
	return time.Time{}, false
}

// ran records the run for the period that starts at start.
func (s *slots) ran(start time.Time) {
	s.done = start
	if !s.pending.After(start) {
		s.pending = time.Time{}
	}
}

// next gives the time of the next check after now.
func (s *slots) next(now time.Time) time.Time {
	if !s.pending.IsZero() {
		return s.last.Add(settleAfterDecode)
	}
	cur := slotStart(now, s.period)
	if t := cur.Add(silentOffset); now.Before(t) {
		return t
	}
	return cur.Add(s.period).Add(silentOffset)
}

// taskText describes a task for the operator page.
func taskText(t hunt.Task) string {
	s := string(t.Kind)
	if t.Call != "" {
		s += " " + t.Call
	}
	if t.Tries > 0 {
		s += fmt.Sprintf(", try %d", t.Tries)
	}
	if t.Note != "" {
		s += ": " + t.Note
	}
	if t.Requested && !t.Decision.OK {
		s += " (refused: " + t.Decision.Reason + ")"
	}
	return s
}

// hunter runs the hunt side. It joins the decodes with the needs,
// publishes the decoded stations and the logged QSOs, and runs the task
// choice and the band foraging once per period.
type hunter struct {
	needs   *hunt.Needs
	feed    *hunt.Feed
	chooser *hunt.Chooser
	forager *bands.Forager
	offsets *bands.Offsets
	now     func() time.Time
	log     *slog.Logger

	slots    slots
	task     atomic.Pointer[string]
	flushing atomic.Bool
	wg       sync.WaitGroup
}

func newHunter(needs *hunt.Needs, feed *hunt.Feed, chooser *hunt.Chooser, forager *bands.Forager, offsets *bands.Offsets, now func() time.Time, log *slog.Logger) *hunter {
	h := &hunter{needs: needs, feed: feed, chooser: chooser, forager: forager, offsets: offsets, now: now, log: log, slots: slots{period: periodFT8}}
	feed.SetLogger(log)
	none := ""
	h.task.Store(&none)
	return h
}

// Task gives the text of the last task.
func (h *hunter) Task() string { return *h.task.Load() }

// run handles the events until ctx ends. Then it waits for the publishes
// that it started.
func (h *hunter) run(ctx context.Context, wsjtxCh <-chan wsjtx.Event, busCh <-chan bus.Event) {
	defer h.wg.Wait()
	timer := time.NewTimer(time.Until(h.slots.next(h.now())))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-wsjtxCh:
			if !ok {
				wsjtxCh = nil
				continue
			}
			h.handleWSJTX(ctx, e)
		case e, ok := <-busCh:
			if !ok {
				busCh = nil
				continue
			}
			if cs, ok := e.(bus.ChaseState); ok {
				h.needs.SetChase(cs)
				h.forager.SetChase(cs)
			}
		case <-timer.C:
		}
		now := h.now()
		if start, ok := h.slots.due(now); ok {
			h.runPeriod(ctx, start)
			now = h.now()
		}
		timer.Reset(h.slots.next(now).Sub(now))
	}
}

func (h *hunter) handleWSJTX(ctx context.Context, e wsjtx.Event) {
	if e.Kind == wsjtx.EventPeerChanged {
		return
	}
	switch m := e.Msg.(type) {
	case wsjtx.Status:
		h.needs.SetStatus(m)
		h.offsets.SetStatus(m)
		h.slots.period = periodOf(m)
	case wsjtx.SpectrumSummary:
		h.offsets.Spectrum(m)
	case wsjtx.Decode:
		if !m.New || m.OffAir || m.Time < 0 {
			return
		}
		// Every decode occupies its offset, also one without a known
		// sender.
		h.offsets.Observe(m)
		st, ok := h.needs.Observe(m, e.At)
		if !ok {
			return
		}
		h.feed.Add(st)
		h.forager.Observe(st)
		if h.slots.decode(decodeStart(e.At, m.Time), e.At) {
			h.chooser.Add(hunt.Heard{Station: st, Decode: m})
		}
	case wsjtx.QSOLogged:
		q, err := h.needs.Logged(m)
		if err != nil {
			h.log.Error("hunt: worked list not saved", "call", q.Call, "err", err)
		}
		h.forager.Logged(q.Call, q.Band, q.Mode)
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			// A failed QSO waits in the feed, and each period tries it again.
			if err := h.feed.PublishQSO(ctx, q); err != nil {
				h.log.Warn("hunt: QSO not published, auto7 tries again each period", "call", q.Call, "err", err)
			}
		}()
	}
}

// runPeriod runs the task choice for the period that starts at start,
// then the band foraging, and publishes the candidates. The forager may
// leave the band only when the task choice found nothing to do.
func (h *hunter) runPeriod(ctx context.Context, start time.Time) {
	t := h.chooser.Run(ctx)
	h.forager.Period(ctx, t.Kind == hunt.TaskNone)
	h.slots.ran(start)
	text := taskText(t)
	if old := h.Task(); text != old {
		h.log.Info("hunt: task", "task", text)
	}
	h.task.Store(&text)
	if h.flushing.Swap(true) {
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer h.flushing.Store(false)
		if err := h.feed.Flush(ctx); err != nil && ctx.Err() == nil {
			h.log.Warn("hunt: candidates or pending QSOs not published", "err", err)
		}
	}()
}
