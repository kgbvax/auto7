// SPDX-License-Identifier: AGPL-3.0-or-later

package hunt

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
)

// fakePub records what the feed publishes.
type fakePub struct {
	cands [][]bus.Candidate
	qsos  []bus.QSO
}

func (p *fakePub) PublishCandidates(_ context.Context, c []bus.Candidate) error {
	p.cands = append(p.cands, append([]bus.Candidate(nil), c...))
	return nil
}

func (p *fakePub) PublishQSO(_ context.Context, q bus.QSO) error {
	p.qsos = append(p.qsos, q)
	return nil
}

func TestFeedPublishesEachDecodedStation(t *testing.T) {
	clk := &fakeClock{t: listNow}
	pub := &fakePub{}
	feed := NewFeed(pub, clk.now)
	feed.Add(Station{Call: "XX2BB", Band: "15m", Mode: "FT8", LastMsg: MsgCQ})
	feed.Add(Station{Call: "XX1AA", Band: "15m", Mode: "FT8", LastMsg: MsgReport})
	feed.Add(Station{Call: "XX1AA", Band: "15m", Mode: "FT8", LastMsg: MsgRR73})
	feed.Add(Station{Call: "XY3CC", Band: "20m", Mode: "FT4", LastMsg: Msg73})
	if err := feed.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []bus.Candidate{
		{Call: "XX1AA", Band: "15m", Mode: "FT8", LastMsg: "RR73"},
		{Call: "XX2BB", Band: "15m", Mode: "FT8", LastMsg: "CQ"},
		{Call: "XY3CC", Band: "20m", Mode: "FT4", LastMsg: "73"},
	}
	if len(pub.cands) != 1 || !reflect.DeepEqual(pub.cands[0], want) {
		t.Fatalf("published %+v, want %+v", pub.cands, want)
	}
}

func TestFeedDropsOldStationsAndClearsTheRetainedList(t *testing.T) {
	clk := &fakeClock{t: listNow}
	pub := &fakePub{}
	feed := NewFeed(pub, clk.now)
	ctx := context.Background()
	if err := feed.Flush(ctx); err != nil || len(pub.cands) != 0 {
		t.Fatalf("empty feed published %+v, %v", pub.cands, err)
	}
	feed.Add(Station{Call: "XX1AA", Band: "15m", Mode: "FT8", LastMsg: MsgCQ})
	_ = feed.Flush(ctx)
	clk.t = listNow.Add(CandidateMaxAge + time.Second)
	_ = feed.Flush(ctx)
	if len(pub.cands) != 2 || len(pub.cands[1]) != 0 {
		t.Fatalf("published %+v, want the list and then an empty list", pub.cands)
	}
	_ = feed.Flush(ctx)
	if len(pub.cands) != 2 {
		t.Fatalf("published %d times, want no repeat of the empty list", len(pub.cands))
	}
}

func TestFeedPublishesLoggedQSO(t *testing.T) {
	clk := &fakeClock{t: listNow}
	pub := &fakePub{}
	feed := NewFeed(pub, clk.now)
	q := bus.QSO{Call: "XX1AA", Band: "15m", FreqHz: 21_075_500, Mode: "FT8", TimeOn: listNow, TimeOff: listNow}
	if err := feed.PublishQSO(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if len(pub.qsos) != 1 || pub.qsos[0] != q {
		t.Fatalf("published %+v", pub.qsos)
	}
}
