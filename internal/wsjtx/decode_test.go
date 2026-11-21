// SPDX-License-Identifier: AGPL-3.0-or-later

package wsjtx

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, b []byte) Message {
	t.Helper()
	m, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return m
}

func TestHeartbeat(t *testing.T) {
	got := mustParse(t, heartbeat("WSJT-X", "a1b2c3-hunter"))
	want := Heartbeat{ID: "WSJT-X", MaxSchema: 3, Version: "2.7.0", Revision: "a1b2c3-hunter"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestBuildFromRevision(t *testing.T) {
	cases := []struct {
		revision string
		want     Build
	}{
		{"a1b2c3-hunter", BuildFull},
		{"-hunter", BuildFull},
		{"a1b2c3", BuildReduced},
		{"a1b2c3-hunterx", BuildReduced},
		{"hunter", BuildReduced},
		{"", BuildReduced},
	}
	for _, c := range cases {
		hb := mustParse(t, heartbeat("WSJT-X", c.revision)).(Heartbeat)
		if got := hb.Build(); got != c.want {
			t.Errorf("revision %q: got %v, want %v", c.revision, got, c.want)
		}
	}
	if BuildFull.String() != "full" || BuildReduced.String() != "reduced" {
		t.Errorf("names: %q %q", BuildFull, BuildReduced)
	}
}

func TestStatus(t *testing.T) {
	got := mustParse(t, statusDatagram(testStatus).b).(Status)
	if got.DialHz != 14074000 || got.Mode != "FT8" || got.TxDF != 1234 ||
		!got.TxEnabled || got.TxMessage != "XX1AA XY3CC AA00" || got.SpecialOp != SpecialOpHound {
		t.Fatalf("key fields wrong: %+v", got)
	}
	if got != testStatus {
		t.Fatalf("got %+v, want %+v", got, testStatus)
	}
}

func TestStatusIgnoresTrailingFields(t *testing.T) {
	// WSJT-X Improved and ws-hunter append fields after Tx Message.
	w := statusDatagram(testStatus)
	w.boolean(true)
	w.utf8("extra")
	w.u32(42)
	if got := mustParse(t, w.b).(Status); got != testStatus {
		t.Fatalf("got %+v, want %+v", got, testStatus)
	}
}

func TestStatusTooShort(t *testing.T) {
	b := statusDatagram(testStatus).b
	if _, err := Parse(b[:len(b)-3]); !errors.Is(err, ErrShort) {
		t.Fatalf("got %v, want ErrShort", err)
	}
}

func TestDecode(t *testing.T) {
	w := newDatagram(2, "WSJT-X")
	w.boolean(true)
	w.qtime(12, 34, 45, 0)
	w.i32(-17)
	w.f64(0.3)
	w.u32(1820)
	w.utf8("~")
	w.utf8("CQ XX2BB AB12")
	w.boolean(false)
	w.boolean(true)
	got := mustParse(t, w.b)
	want := Decode{ID: "WSJT-X", New: true, Time: tod(12, 34, 45, 0), SNR: -17, DeltaTime: 0.3,
		DeltaFreq: 1820, Mode: "~", Message: "CQ XX2BB AB12", LowConfidence: false, OffAir: true}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestClearAndClose(t *testing.T) {
	if got := mustParse(t, newDatagram(3, "WSJT-X").b); got != (Clear{ID: "WSJT-X"}) {
		t.Fatalf("clear: got %+v", got)
	}
	if got := mustParse(t, newDatagram(6, "WSJT-X").b); got != (Close{ID: "WSJT-X"}) {
		t.Fatalf("close: got %+v", got)
	}
}

func TestQSOLogged(t *testing.T) {
	w := newDatagram(5, "WSJT-X")
	w.qdatetimeUTC(2461114, 10, 21, 30) // 2026-03-14
	w.utf8("XX2BB")
	w.utf8("AB12")
	w.u64(7075500)
	w.utf8("FT4")
	w.utf8("-05")
	w.utf8("-09")
	w.utf8("100")
	w.utf8("")
	w.null()
	// Date & Time On with an offset of +3600 s.
	w.i64(2461114)
	w.qtime(11, 20, 0, 0)
	w.u8(2)
	w.i32(3600)
	w.utf8("")
	w.utf8("XY3CC")
	w.utf8("AA00")
	w.utf8("")
	w.utf8("")
	w.utf8("")
	got := mustParse(t, w.b)
	want := QSOLogged{
		ID:       "WSJT-X",
		TimeOff:  time.Date(2026, 3, 14, 10, 21, 30, 0, time.UTC),
		DXCall:   "XX2BB",
		DXGrid:   "AB12",
		TxFreqHz: 7075500,
		Mode:     "FT4", ReportSent: "-05", ReportReceived: "-09", TxPower: "100",
		TimeOn: time.Date(2026, 3, 14, 10, 20, 0, 0, time.UTC),
		MyCall: "XY3CC", MyGrid: "AA00",
	}
	g := got.(QSOLogged)
	if !g.TimeOn.Equal(want.TimeOn) {
		t.Fatalf("time on: got %v, want %v", g.TimeOn, want.TimeOn)
	}
	g.TimeOn = want.TimeOn
	if !reflect.DeepEqual(g, want) {
		t.Fatalf("got %+v, want %+v", g, want)
	}
}

func TestLoggedADIFLongerThan1024Bytes(t *testing.T) {
	adif := "<adif_ver:5>3.1.0<EOH>" + strings.Repeat("<comment:4>XX1A", 150) + "<EOR>"
	if len(adif) <= 1024 {
		t.Fatalf("test text is too short: %d", len(adif))
	}
	w := newDatagram(12, "WSJT-X")
	w.utf8(adif)
	got := mustParse(t, w.b)
	if got != (LoggedADIF{ID: "WSJT-X", ADIF: adif}) {
		t.Fatalf("ADIF text not decoded in full")
	}
}

func TestWSPRDecode(t *testing.T) {
	w := newDatagram(10, "WSJT-X")
	w.boolean(true)
	w.qtime(1, 2, 0, 0)
	w.i32(-25)
	w.f64(-1.5)
	w.u64(14097050)
	w.i32(-1)
	w.utf8("XX1AA")
	w.utf8("AA00")
	w.i32(37)
	w.boolean(false)
	got := mustParse(t, w.b)
	want := WSPRDecode{ID: "WSJT-X", New: true, Time: tod(1, 2, 0, 0), SNR: -25, DeltaTime: -1.5,
		FrequencyHz: 14097050, Drift: -1, Callsign: "XX1AA", Grid: "AA00", PowerDBm: 37}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSpectrumSummary(t *testing.T) {
	w := newDatagram(27, "WSJT-X")
	w.qtime(18, 0, 15, 0)
	w.u32(15000)
	w.u32(200)
	w.u32(50)
	w.f64(-120)
	w.f64(0.5)
	w.bytes([]byte{0, 10, 255})
	w.u32(7) // a trailing field from a later version
	got := mustParse(t, w.b).(SpectrumSummary)
	want := SpectrumSummary{ID: "WSJT-X", PeriodStart: tod(18, 0, 15, 0), TRPeriodMs: 15000,
		StartHz: 200, BinWidthHz: 50, FloorDB: -120, StepDB: 0.5, Levels: []byte{0, 10, 255}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if hz, db := got.Bin(1); hz != 250 || db != -115 {
		t.Errorf("bin 1: got %d Hz %v dB, want 250 Hz -115 dB", hz, db)
	}
	if hz, db := got.Bin(2); hz != 300 || db != 7.5 {
		t.Errorf("bin 2: got %d Hz %v dB, want 300 Hz 7.5 dB", hz, db)
	}
}

func TestNullTime(t *testing.T) {
	w := newDatagram(2, "WSJT-X")
	w.boolean(true)
	w.u32(0xffffffff)
	w.i32(0)
	w.f64(0)
	w.u32(0)
	w.utf8("~")
	w.utf8("")
	w.boolean(false)
	w.boolean(false)
	if got := mustParse(t, w.b).(Decode); got.Time >= 0 {
		t.Fatalf("null time: got %v, want a negative value", got.Time)
	}
}

func TestUnknownType(t *testing.T) {
	_, err := Parse(newDatagram(17, "WSJT-X").b)
	if !errors.Is(err, ErrUnknownType) {
		t.Fatalf("got %v, want ErrUnknownType", err)
	}
}

func TestBadHeader(t *testing.T) {
	b := heartbeat("WSJT-X", "a1b2c3")
	bad := append([]byte(nil), b...)
	bad[0] = 0
	if _, err := Parse(bad); !errors.Is(err, ErrNotWSJTX) {
		t.Errorf("magic: got %v, want ErrNotWSJTX", err)
	}
	bad = append([]byte(nil), b...)
	bad[7] = 9
	if _, err := Parse(bad); !errors.Is(err, ErrSchema) {
		t.Errorf("schema: got %v, want ErrSchema", err)
	}
	if _, err := Parse(b[:6]); !errors.Is(err, ErrShort) {
		t.Errorf("short: got %v, want ErrShort", err)
	}
}
