// SPDX-License-Identifier: AGPL-3.0-or-later

package hunt

import (
	"cmp"
	"context"
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
}

// NewFeed returns a Feed that publishes through pub.
func NewFeed(pub Publisher, now func() time.Time) *Feed {
	return &Feed{pub: pub, now: now, cands: map[key]heard{}}
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
// publishes nothing while the list stays empty. Call it once per period.
func (f *Feed) Flush(ctx context.Context) error {
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

// PublishQSO publishes one logged QSO on the QSO topic.
func (f *Feed) PublishQSO(ctx context.Context, q bus.QSO) error {
	return f.pub.PublishQSO(ctx, q)
}
