// SPDX-License-Identifier: AGPL-3.0-or-later

package wsjtx

import (
	"encoding/binary"
	"math"
	"time"
)

// builder writes test datagrams field by field. It does not use the
// decoder of this package, so a test compares the decoder against an
// independent byte layout.
type builder struct{ b []byte }

// newDatagram starts a datagram with the magic, schema 3, the type and the
// Id.
func newDatagram(typ uint32, id string) *builder {
	w := &builder{}
	w.u32(0xadbccbda)
	w.u32(3)
	w.u32(typ)
	w.utf8(id)
	return w
}

func (w *builder) u8(v uint8)    { w.b = append(w.b, v) }
func (w *builder) u32(v uint32)  { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *builder) i32(v int32)   { w.u32(uint32(v)) }
func (w *builder) u64(v uint64)  { w.b = binary.BigEndian.AppendUint64(w.b, v) }
func (w *builder) i64(v int64)   { w.u64(uint64(v)) }
func (w *builder) f64(v float64) { w.u64(math.Float64bits(v)) }

func (w *builder) boolean(v bool) {
	if v {
		w.u8(1)
	} else {
		w.u8(0)
	}
}

func (w *builder) utf8(s string) {
	w.u32(uint32(len(s)))
	w.b = append(w.b, s...)
}

// null writes a null QByteArray.
func (w *builder) null() { w.u32(0xffffffff) }

func (w *builder) bytes(p []byte) {
	w.u32(uint32(len(p)))
	w.b = append(w.b, p...)
}

// qtime writes milliseconds since midnight.
func (w *builder) qtime(h, m, s, ms int) {
	w.u32(uint32(((h*60+m)*60+s)*1000 + ms))
}

// qdatetimeUTC writes a QDateTime with timespec UTC. jd is the Julian day
// number.
func (w *builder) qdatetimeUTC(jd int64, h, m, s int) {
	w.i64(jd)
	w.qtime(h, m, s, 0)
	w.u8(1)
}

// tod is a time of day as a duration since midnight.
func tod(h, m, s, ms int) time.Duration {
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute +
		time.Duration(s)*time.Second + time.Duration(ms)*time.Millisecond
}

func heartbeat(id, revision string) []byte {
	w := newDatagram(0, id)
	w.u32(3)
	w.utf8("2.7.0")
	w.utf8(revision)
	return w.b
}

// testStatus is the invented content of the Status datagrams below.
var testStatus = Status{
	ID:            "WSJT-X",
	DialHz:        14074000,
	Mode:          "FT8",
	DXCall:        "XX1AA",
	Report:        "-12",
	TxMode:        "FT8",
	TxEnabled:     true,
	Transmitting:  false,
	Decoding:      true,
	RxDF:          1500,
	TxDF:          1234,
	DECall:        "XY3CC",
	DEGrid:        "AA00",
	DXGrid:        "AB11",
	TxWatchdog:    false,
	SubMode:       "",
	FastMode:      false,
	SpecialOp:     SpecialOpHound,
	FreqTolerance: math.MaxUint32,
	TRPeriod:      15,
	ConfigName:    "Default",
	TxMessage:     "XX1AA XY3CC AA00",
}

// statusDatagram writes s in the upstream field order.
func statusDatagram(s Status) *builder {
	w := newDatagram(1, s.ID)
	w.u64(s.DialHz)
	w.utf8(s.Mode)
	w.utf8(s.DXCall)
	w.utf8(s.Report)
	w.utf8(s.TxMode)
	w.boolean(s.TxEnabled)
	w.boolean(s.Transmitting)
	w.boolean(s.Decoding)
	w.u32(s.RxDF)
	w.u32(s.TxDF)
	w.utf8(s.DECall)
	w.utf8(s.DEGrid)
	w.utf8(s.DXGrid)
	w.boolean(s.TxWatchdog)
	w.utf8(s.SubMode)
	w.boolean(s.FastMode)
	w.u8(uint8(s.SpecialOp))
	w.u32(s.FreqTolerance)
	w.u32(s.TRPeriod)
	w.utf8(s.ConfigName)
	w.utf8(s.TxMessage)
	return w
}
