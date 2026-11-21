// SPDX-License-Identifier: AGPL-3.0-or-later

package hunt

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

func TestParseMessage(t *testing.T) {
	cases := []struct {
		text string
		want Message
	}{
		{"CQ XX1AA AA00", Message{Type: MsgCQ, From: "XX1AA", Grid: "AA00"}},
		{"CQ DX XX1AA AA00", Message{Type: MsgCQ, From: "XX1AA", Grid: "AA00", CQModifier: "DX"}},
		{"CQ XX1AA", Message{Type: MsgCQ, From: "XX1AA"}},
		{"CQ XX1AA/P AA00", Message{Type: MsgCQ, From: "XX1AA/P", Grid: "AA00"}},
		{"XX2BB XX1AA AA00", Message{Type: MsgGrid, From: "XX1AA", To: "XX2BB", Grid: "AA00"}},
		{"XX2BB XX1AA -10", Message{Type: MsgReport, From: "XX1AA", To: "XX2BB", Report: "-10"}},
		{"XX2BB XX1AA R+05", Message{Type: MsgReport, From: "XX1AA", To: "XX2BB", Report: "R+05"}},
		{"XX2BB XX1AA RR73", Message{Type: MsgRR73, From: "XX1AA", To: "XX2BB"}},
		{"XX2BB XX1AA RRR", Message{Type: MsgRRR, From: "XX1AA", To: "XX2BB"}},
		{"XX2BB XX1AA 73", Message{Type: Msg73, From: "XX1AA", To: "XX2BB"}},
		{"<XX2BB> XX1AA RR73", Message{Type: MsgRR73, From: "XX1AA", To: "XX2BB"}},
		{"xx2bb xx1aa rr73", Message{Type: MsgRR73, From: "XX1AA", To: "XX2BB"}},
		{"XX1AA RR73; XX2BB <XY3CC> -10", Message{Type: MsgRR73, From: "XY3CC", To: "XX1AA", Second: "XX2BB", Report: "-10", FoxForm: true}},
	}
	for _, c := range cases {
		got, ok := ParseMessage(c.text)
		if !ok || got != c.want {
			t.Errorf("ParseMessage(%q) = %+v, %v, want %+v", c.text, got, ok, c.want)
		}
	}
	for _, bad := range []string{"", "TNX 73 GL", "CQ DX", "XX2BB <...> RR73", "CQ AA00"} {
		if got, ok := ParseMessage(bad); ok {
			t.Errorf("ParseMessage(%q) = %+v, want no sender", bad, got)
		}
	}
}

func TestBandOf(t *testing.T) {
	cases := map[uint64]string{
		1_840_000: "160m", 3_573_000: "80m", 7_074_000: "40m", 10_136_000: "30m",
		14_074_000: "20m", 18_100_000: "17m", 21_074_000: "15m", 24_915_000: "12m",
		28_074_000: "10m", 50_313_000: "6m", 0: "", 144_174_000: "",
	}
	for hz, want := range cases {
		if got := BandOf(hz); got != want {
			t.Errorf("BandOf(%d) = %q, want %q", hz, got, want)
		}
	}
}

// fixture builds a Needs on a temporary state directory.
type fixture struct {
	clk   *fakeClock
	dir   string
	needs *Needs
}

func newFixture(t *testing.T, foxDials ...uint64) *fixture {
	t.Helper()
	f := &fixture{clk: &fakeClock{t: listNow}, dir: t.TempDir()}
	f.reopen(t, foxDials...)
	return f
}

func (f *fixture) reopen(t *testing.T, foxDials ...uint64) {
	t.Helper()
	l, err := OpenLists(f.dir, 10*time.Minute, f.clk.now)
	if err != nil {
		t.Fatal(err)
	}
	f.needs = NewNeeds(l, foxDials)
}

func status(dialHz uint64) wsjtx.Status {
	return wsjtx.Status{DialHz: dialHz, Mode: "FT8"}
}

func decode(msg string) wsjtx.Decode {
	return wsjtx.Decode{New: true, Time: 12 * time.Hour, SNR: -10, DeltaFreq: 1200, Mode: "~", Message: msg}
}

func chase(rows ...bus.ChaseRow) bus.ChaseState { return bus.ChaseState{Ranked: rows} }

func TestATNONeedIsHighValue(t *testing.T) {
	f := newFixture(t)
	f.needs.SetStatus(status(21_074_000))
	f.needs.SetChase(chase(bus.ChaseRow{Call: "XX1AA", Band: "15m", Needed: true,
		Needs: []bus.ChaseNeed{{Award: "dxcc", Key: "1", Status: "unworked", ATNO: true}}}))
	s, ok := f.needs.Observe(decode("CQ XX1AA AA00"), listNow)
	if !ok || !s.Needed || !s.HighValue {
		t.Fatalf("station = %+v, %v, want needed and high value", s, ok)
	}
	if s.Band != "15m" || s.Mode != "FT8" || s.LastMsg != MsgCQ || s.Row == nil {
		t.Fatalf("station = %+v", s)
	}
}

func TestFirstTierExpeditionIsHighValue(t *testing.T) {
	f := newFixture(t)
	f.needs.SetStatus(status(21_074_000))
	f.needs.SetChase(chase(bus.ChaseRow{Call: "XX1AA", Band: "15m", Needed: true,
		Expedition: &bus.ChaseExpedition{Call: "XX1AA", Tier: "first"}}))
	s, ok := f.needs.Observe(decode("CQ XX1AA AA00"), listNow)
	if !ok || !s.Needed || !s.HighValue {
		t.Fatalf("station = %+v, want high value", s)
	}
}

func TestNeedOnOtherBandIsNotNeeded(t *testing.T) {
	f := newFixture(t)
	f.needs.SetStatus(status(21_074_000))
	f.needs.SetChase(chase(
		bus.ChaseRow{Call: "XX1AA", Band: "20m", Needed: true},
		bus.ChaseRow{Call: "XX2BB", Band: "15m", Needed: false},
		bus.ChaseRow{Call: "XY3CC", Band: "15m", Needed: true}))
	for _, call := range []string{"XX1AA", "XX2BB"} {
		s, _ := f.needs.Observe(decode("CQ "+call+" AA00"), listNow)
		if s.Needed || s.HighValue {
			t.Errorf("%s = %+v, want not needed", call, s)
		}
	}
	s, _ := f.needs.Observe(decode("CQ XY3CC AA00"), listNow)
	if !s.Needed || s.HighValue {
		t.Errorf("XY3CC = %+v, want needed and not high value", s)
	}
}

func TestLoggedQSOEndsNeedOnBandAlsoAfterRestart(t *testing.T) {
	f := newFixture(t)
	rows := chase(bus.ChaseRow{Call: "XX1AA", Band: "15m", Needed: true},
		bus.ChaseRow{Call: "XX1AA", Band: "20m", Needed: true})
	f.needs.SetStatus(status(21_074_000))
	f.needs.SetChase(rows)
	q, err := f.needs.Logged(wsjtx.QSOLogged{DXCall: "XX1AA", DXGrid: "AA00", TxFreqHz: 21_075_500,
		Mode: "FT8", TimeOn: listNow.Add(-time.Minute), TimeOff: listNow})
	if err != nil {
		t.Fatal(err)
	}
	if q.Call != "XX1AA" || q.Band != "15m" || q.Mode != "FT8" || q.FreqHz != 21_075_500 {
		t.Fatalf("QSO = %+v", q)
	}
	check := func(when string) {
		t.Helper()
		f.needs.SetStatus(status(21_074_000))
		f.needs.SetChase(rows)
		s, _ := f.needs.Observe(decode("CQ XX1AA AA00"), listNow)
		if s.Needed || !s.Worked {
			t.Fatalf("%s: 15m station = %+v, want worked and not needed", when, s)
		}
		f.needs.SetStatus(status(14_074_000))
		s, _ = f.needs.Observe(decode("CQ XX1AA AA00"), listNow)
		if !s.Needed || s.Worked {
			t.Fatalf("%s: 20m station = %+v, want needed", when, s)
		}
	}
	check("before restart")
	f.reopen(t)
	check("after restart")
}

func TestSkippedStationIsMarked(t *testing.T) {
	f := newFixture(t)
	f.needs.SetStatus(status(21_074_000))
	if err := f.needs.Lists().Skip("XX1AA"); err != nil {
		t.Fatal(err)
	}
	s, _ := f.needs.Observe(decode("CQ XX1AA AA00"), listNow)
	if !s.Skipped {
		t.Fatalf("station = %+v, want skipped", s)
	}
}

func TestSpotOnFoxHoundDialIsHighlightOnly(t *testing.T) {
	f := newFixture(t, 14_090_000)
	if !f.needs.SpotHighlightOnly(bus.ChaseRow{Call: "XX1AA", Band: "20m", FreqHz: 14_090_000}) {
		t.Fatal("spot on 14090 kHz must be highlight only")
	}
	if !f.needs.SpotHighlightOnly(bus.ChaseRow{Call: "XX1AA", Band: "20m", FreqHz: 14_090_600}) {
		t.Fatal("spot in the passband above 14090 kHz must be highlight only")
	}
	if f.needs.SpotHighlightOnly(bus.ChaseRow{Call: "XX1AA", Band: "20m", FreqHz: 14_074_000}) {
		t.Fatal("spot on 14074 kHz must not be highlight only")
	}
	f.needs.SetStatus(status(14_090_000))
	s, _ := f.needs.Observe(decode("CQ XX1AA AA00"), listNow)
	if !s.HighlightOnly || s.Highlight != HighlightFoxHoundDial {
		t.Fatalf("decode on the Fox/Hound dial = %+v, want highlight only", s)
	}
}

func TestFoxCompoundFormMarksFoxForVisit(t *testing.T) {
	f := newFixture(t, 14_090_000)
	f.needs.SetStatus(status(21_074_000))
	s, ok := f.needs.Observe(decode("XX1AA RR73; XX2BB <XY3CC> -10"), listNow)
	if !ok || s.Call != "XY3CC" || !s.HighlightOnly || s.Highlight != HighlightFoxForm {
		t.Fatalf("fox = %+v, %v, want XY3CC highlight only", s, ok)
	}
	s, _ = f.needs.Observe(decode("CQ XY3CC AA00"), listNow)
	if !s.HighlightOnly {
		t.Fatalf("later CQ of the fox in the same visit = %+v, want highlight only", s)
	}
	s, _ = f.needs.Observe(decode("CQ XX1AA AA00"), listNow)
	if s.HighlightOnly {
		t.Fatalf("other station = %+v, want normal", s)
	}
	f.needs.SetStatus(status(14_074_000))
	f.needs.SetStatus(status(21_074_000))
	s, _ = f.needs.Observe(decode("CQ XY3CC AA00"), listNow)
	if s.HighlightOnly {
		t.Fatalf("fox in a new visit = %+v, want normal", s)
	}
}

func TestSpecialOpMakesEveryDecodeHighlightOnly(t *testing.T) {
	f := newFixture(t)
	st := status(21_074_000)
	st.SpecialOp = wsjtx.SpecialOpHound
	f.needs.SetStatus(st)
	s, _ := f.needs.Observe(decode("CQ XX1AA AA00"), listNow)
	if !s.HighlightOnly || s.Highlight != HighlightSpecialOp {
		t.Fatalf("station = %+v, want highlight only", s)
	}
}

func TestObserveSkipsDecodesWithoutBandModeOrSender(t *testing.T) {
	f := newFixture(t)
	if _, ok := f.needs.Observe(decode("CQ XX1AA AA00"), listNow); ok {
		t.Fatal("decode before any Status must be skipped")
	}
	f.needs.SetStatus(wsjtx.Status{DialHz: 21_074_000, Mode: "JT65"})
	d := decode("CQ XX1AA AA00")
	d.Mode = "#"
	if _, ok := f.needs.Observe(d, listNow); ok {
		t.Fatal("JT65 decode must be skipped")
	}
	f.needs.SetStatus(status(21_074_000))
	if _, ok := f.needs.Observe(decode("TNX 73 GL"), listNow); ok {
		t.Fatal("free text must be skipped")
	}
	d = decode("CQ XX1AA AA00")
	d.Mode = "+"
	s, ok := f.needs.Observe(d, listNow)
	if !ok || s.Mode != "FT4" {
		t.Fatalf("FT4 decode = %+v, %v", s, ok)
	}
}

func TestUnreadableListsReportedByNeeds(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lists.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: listNow}
	l, _ := OpenLists(dir, 10*time.Minute, clk.now)
	n := NewNeeds(l, nil)
	if err := n.ListsErr(); !errors.Is(err, ErrListsUnreadable) || !strings.HasPrefix(err.Error(), ListsUnreadable) {
		t.Fatalf("ListsErr() = %v, want lists unreadable", err)
	}
}
