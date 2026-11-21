// SPDX-License-Identifier: AGPL-3.0-or-later

package hunt

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
)

// Publisher publishes the candidates and the logged QSOs. bus.Client
// implements it.
type Publisher interface {
	PublishCandidates(ctx context.Context, cands []bus.Candidate) error
	PublishQSO(ctx context.Context, q bus.QSO) error
}

// CandidateMaxAge is the time that a decoded station stays on the
// candidate list after its last decode.
const CandidateMaxAge = 10 * time.Minute

// heard is one station on the candidate list.
type heard struct {
	cand bus.Candidate
	at   time.Time
}

// Feed keeps the stations that auto7 decoded and publishes them as
// candidates. All methods are safe for use by many goroutines.
type Feed struct {
	pub Publisher
	now func() time.Time

	mu    sync.Mutex
	cands map[key]heard
	// shown is true when the last published list was not empty.
	shown bool
	// log gets the warning when the pending list drops a QSO.
	log *slog.Logger
	// pending holds the logged QSOs that wait for a publish, oldest
	// first. seq numbers them.
	pending []pendingQSO
	seq     uint64

	// sending lets only one goroutine at a time publish the pending
	// QSOs, so a QSO goes out once per success.
	sending sync.Mutex
}

// pendingQSO is one logged QSO that waits for a publish.
type pendingQSO struct {
	seq uint64
	qso bus.QSO
}

// NewFeed returns a Feed that publishes through pub.
func NewFeed(pub Publisher, now func() time.Time) *Feed {
	return &Feed{pub: pub, now: now, cands: map[key]heard{}, log: slog.Default()}
}

// SetLogger sets the logger of the feed. A nil logger keeps the old one.
func (f *Feed) SetLogger(l *slog.Logger) {
	if l == nil {
		return
	}
	f.mu.Lock()
	f.log = l
	f.mu.Unlock()
}

// Add records a decoded station with the type of its last message.
func (f *Feed) Add(s Station) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cands[key{s.Call, s.Band}] = heard{
		cand: bus.Candidate{Call: s.Call, Band: s.Band, Mode: s.Mode, LastMsg: string(s.LastMsg)},
		at:   f.now(),
	}
}

// Flush publishes the stations heard in the last CandidateMaxAge, sorted
// by band and call. When the list becomes empty, it publishes the empty
// list once, so the retained list does not keep old stations. It
// publishes nothing while the list stays empty. Then it publishes the
// logged QSOs that wait for a publish. Call it once per period.
func (f *Feed) Flush(ctx context.Context) error {
	return errors.Join(f.flushCandidates(ctx), f.sendQSOs(ctx))
}

// flushCandidates publishes the candidate list for Flush.
func (f *Feed) flushCandidates(ctx context.Context) error {
	f.mu.Lock()
	now := f.now()
	list := make([]bus.Candidate, 0, len(f.cands))
	for k, h := range f.cands {
		if now.Sub(h.at) > CandidateMaxAge {
			delete(f.cands, k)
			continue
		}
		list = append(list, h.cand)
	}
	if len(list) == 0 && !f.shown {
		f.mu.Unlock()
		return nil
	}
	f.shown = len(list) > 0
	f.mu.Unlock()
	slices.SortFunc(list, func(a, b bus.Candidate) int {
		return cmp.Or(cmp.Compare(a.Band, b.Band), cmp.Compare(a.Call, b.Call))
	})
	return f.pub.PublishCandidates(ctx, list)
}

// PublishQSO publishes one logged QSO on the QSO topic. The QSO waits
// on the pending list until a publish succeeds, and each Flush tries it
// again. The pending list is in memory only. It holds at most
// MaxPendingQSOs, and a full list drops its oldest QSO.
func (f *Feed) PublishQSO(ctx context.Context, q bus.QSO) error {
	f.mu.Lock()
	f.seq++
	f.pending = append(f.pending, pendingQSO{seq: f.seq, qso: q})
	if n := len(f.pending) - MaxPendingQSOs; n > 0 {
		for _, p := range f.pending[:n] {
			f.log.Warn("hunt: pending list full, QSO dropped", "call", p.qso.Call, "band", p.qso.Band)
		}
		f.pending = slices.Delete(f.pending, 0, n)
	}
	f.mu.Unlock()
	return f.sendQSOs(ctx)
}

// sendQSOs publishes the pending QSOs, oldest first. It stops at the
// first error and keeps the QSOs that did not go out.
func (f *Feed) sendQSOs(ctx context.Context) error {
	f.sending.Lock()
	defer f.sending.Unlock()
	for {
		f.mu.Lock()
		if len(f.pending) == 0 {
			f.mu.Unlock()
			return nil
		}
		p := f.pending[0]
		f.mu.Unlock()
		if err := f.pub.PublishQSO(ctx, p.qso); err != nil {
			return err
		}
		f.mu.Lock()
		// A full list can drop the QSO during the publish, so remove it
		// by its number.
		f.pending = slices.DeleteFunc(f.pending, func(x pendingQSO) bool { return x.seq == p.seq })
		f.mu.Unlock()
	}
}

// MaxPendingQSOs is the most logged QSOs that wait for a publish.
const MaxPendingQSOs = 500
