// SPDX-License-Identifier: AGPL-3.0-or-later

// Package wsjtx receives and decodes the datagrams that WSJT-X sends over
// UDP. It reads the WSJT-X protocol with schema 3 and the message types of
// the ws-hunter build. The package has no send function. The command
// encoders live in the TX control code.
package wsjtx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Magic is the first quint32 of every WSJT-X datagram.
const Magic uint32 = 0xadbccbda

// Schema is the schema number that auto7 uses. The decoder also accepts
// schema 2, which has the same layout for every field that auto7 reads.
const Schema uint32 = 3

// MaxDatagram is the largest datagram that the receiver reads.
const MaxDatagram = 65536

// Message types of the WSJT-X protocol and of ws-hunter. The decoder reads
// only the types that WSJT-X sends. The other types are commands to WSJT-X.
const (
	TypeHeartbeat        uint32 = 0
	TypeStatus           uint32 = 1
	TypeDecode           uint32 = 2
	TypeClear            uint32 = 3
	TypeReply            uint32 = 4
	TypeQSOLogged        uint32 = 5
	TypeClose            uint32 = 6
	TypeReplay           uint32 = 7
	TypeHaltTx           uint32 = 8
	TypeFreeText         uint32 = 9
	TypeWSPRDecode       uint32 = 10
	TypeLocation         uint32 = 11
	TypeLoggedADIF       uint32 = 12
	TypeHighlightCall    uint32 = 13
	TypeSwitchConfig     uint32 = 14
	TypeConfigure        uint32 = 15
	TypeAnnotationInfo   uint32 = 16
	TypeSetTxPeriod      uint32 = 19
	TypeSetDialFrequency uint32 = 20
	TypeSetTxDF          uint32 = 24
	TypeSetHoldTxFreq    uint32 = 25
	TypeSetTxEnabled     uint32 = 26
	TypeSpectrumSummary  uint32 = 27
	TypeClearDX          uint32 = 28
)

// Decode errors. Parse wraps them, so use errors.Is.
var (
	// ErrNotWSJTX means that the datagram does not start with Magic.
	ErrNotWSJTX = errors.New("wsjtx: no WSJT-X magic number")
	// ErrSchema means that the schema number is not 2 or 3.
	ErrSchema = errors.New("wsjtx: unsupported schema")
	// ErrShort means that the datagram ends before the last known field.
	ErrShort = errors.New("wsjtx: datagram too short")
	// ErrUnknownType means that the decoder does not read this type.
	ErrUnknownType = errors.New("wsjtx: unknown message type")
)

// Message is one decoded datagram.
type Message interface {
	// InstanceID is the Id of the WSJT-X instance that sent the message.
	InstanceID() string
}

// Build says which commands the WSJT-X build accepts.
type Build int

const (
	// BuildReduced is a stock WSJT-X build. auto7 runs in reduced mode.
	BuildReduced Build = iota
	// BuildFull is a ws-hunter build. It accepts the ws-hunter commands.
	BuildFull
)

// HunterSuffix is the end of the Heartbeat revision of a ws-hunter build.
const HunterSuffix = "-hunter"

// BuildOf gives the build kind of a Heartbeat revision.
func BuildOf(revision string) Build {
	if strings.HasSuffix(revision, HunterSuffix) {
		return BuildFull
	}
	return BuildReduced
}

func (b Build) String() string {
	if b == BuildFull {
		return "full"
	}
	return "reduced"
}

// SpecialOp is the Special Operation Mode of the Status message.
type SpecialOp uint8

// Special operation modes.
const (
	SpecialOpNone     SpecialOp = 0
	SpecialOpNAVHF    SpecialOp = 1
	SpecialOpEUVHF    SpecialOp = 2
	SpecialOpFieldDay SpecialOp = 3
	SpecialOpRTTYRU   SpecialOp = 4
	SpecialOpWWDigi   SpecialOp = 5
	SpecialOpFox      SpecialOp = 6
	SpecialOpHound    SpecialOp = 7
	SpecialOpARRLDigi SpecialOp = 8
)

// NotApplicable is the value of Status.FreqTolerance and Status.TRPeriod
// when the field does not apply.
const NotApplicable = math.MaxUint32

// Heartbeat is message type 0.
type Heartbeat struct {
	ID        string
	MaxSchema uint32
	Version   string
	Revision  string
}

// Build gives the build kind from the revision.
func (h Heartbeat) Build() Build { return BuildOf(h.Revision) }

// Status is message type 1. WSJT-X Improved and ws-hunter send more fields
// after TxMessage. The decoder ignores them.
type Status struct {
	ID           string
	DialHz       uint64
	Mode         string
	DXCall       string
	Report       string
	TxMode       string
	TxEnabled    bool
	Transmitting bool
	Decoding     bool
	RxDF         uint32
	TxDF         uint32
	DECall       string
	DEGrid       string
	DXGrid       string
	TxWatchdog   bool
	SubMode      string
	FastMode     bool
	SpecialOp    SpecialOp
	// FreqTolerance is NotApplicable when the mode has none.
	FreqTolerance uint32
	// TRPeriod is the period in seconds. It is NotApplicable when the mode
	// has none.
	TRPeriod   uint32
	ConfigName string
	TxMessage  string
}

// Decode is message type 2.
type Decode struct {
	ID  string
	New bool
	// Time is the start of the period since midnight UTC. It is negative
	// when WSJT-X sent a null time.
	Time          time.Duration
	SNR           int32
	DeltaTime     float64
	DeltaFreq     uint32
	Mode          string
	Message       string
	LowConfidence bool
	OffAir        bool
}

// Clear is message type 3. WSJT-X erased the Band Activity window.
type Clear struct{ ID string }

// QSOLogged is message type 5.
type QSOLogged struct {
	ID             string
	TimeOff        time.Time
	DXCall         string
	DXGrid         string
	TxFreqHz       uint64
	Mode           string
	ReportSent     string
	ReportReceived string
	TxPower        string
	Comments       string
	Name           string
	TimeOn         time.Time
	OperatorCall   string
	MyCall         string
	MyGrid         string
	ExchangeSent   string
	ExchangeRcvd   string
	PropMode       string
}

// Close is message type 6. WSJT-X shuts down.
type Close struct{ ID string }

// WSPRDecode is message type 10.
type WSPRDecode struct {
	ID          string
	New         bool
	Time        time.Duration
	SNR         int32
	DeltaTime   float64
	FrequencyHz uint64
	Drift       int32
	Callsign    string
	Grid        string
	PowerDBm    int32
	OffAir      bool
}

// LoggedADIF is message type 12.
type LoggedADIF struct {
	ID   string
	ADIF string
}

// SpectrumSummary is the ws-hunter message type 27. ws-hunter sends it
// after each receive period. It never sends it for a period in which the
// station transmitted.
type SpectrumSummary struct {
	ID string
	// PeriodStart is the start of the period since midnight UTC.
	PeriodStart time.Duration
	TRPeriodMs  uint32
	StartHz     uint32
	BinWidthHz  uint32
	FloorDB     float64
	StepDB      float64
	// Levels holds one level for each bin.
	Levels []byte
}

// Bin gives the audio frequency in Hz and the level in dB of bin i. i must
// be less than len(Levels).
func (s SpectrumSummary) Bin(i int) (hz uint32, db float64) {
	return s.StartHz + uint32(i)*s.BinWidthHz, s.FloorDB + float64(s.Levels[i])*s.StepDB
}

func (m Heartbeat) InstanceID() string       { return m.ID }
func (m Status) InstanceID() string          { return m.ID }
func (m Decode) InstanceID() string          { return m.ID }
func (m Clear) InstanceID() string           { return m.ID }
func (m QSOLogged) InstanceID() string       { return m.ID }
func (m Close) InstanceID() string           { return m.ID }
func (m WSPRDecode) InstanceID() string      { return m.ID }
func (m LoggedADIF) InstanceID() string      { return m.ID }
func (m SpectrumSummary) InstanceID() string { return m.ID }

// Parse decodes one datagram. It ignores bytes after the last known field.
// For a type that it does not read, it returns an error that wraps
// ErrUnknownType.
func Parse(b []byte) (Message, error) {
	r := &reader{b: b}
	if r.u32() != Magic {
		if r.err != nil {
			return nil, r.err
		}
		return nil, ErrNotWSJTX
	}
	schema := r.u32()
	if r.err == nil && schema != 2 && schema != 3 {
		return nil, fmt.Errorf("%w: %d", ErrSchema, schema)
	}
	typ := r.u32()
	id := r.utf8()
	if r.err != nil {
		return nil, r.err
	}
	var m Message
	switch typ {
	case TypeHeartbeat:
		m = Heartbeat{ID: id, MaxSchema: r.u32(), Version: r.utf8(), Revision: r.utf8()}
	case TypeStatus:
		m = r.status(id)
	case TypeDecode:
		m = Decode{ID: id, New: r.boolean(), Time: r.qtime(), SNR: r.i32(), DeltaTime: r.f64(),
			DeltaFreq: r.u32(), Mode: r.utf8(), Message: r.utf8(), LowConfidence: r.boolean(), OffAir: r.boolean()}
	case TypeClear:
		m = Clear{ID: id}
	case TypeQSOLogged:
		m = r.qsoLogged(id)
	case TypeClose:
		m = Close{ID: id}
	case TypeWSPRDecode:
		m = WSPRDecode{ID: id, New: r.boolean(), Time: r.qtime(), SNR: r.i32(), DeltaTime: r.f64(),
			FrequencyHz: r.u64(), Drift: r.i32(), Callsign: r.utf8(), Grid: r.utf8(), PowerDBm: r.i32(), OffAir: r.boolean()}
	case TypeLoggedADIF:
		m = LoggedADIF{ID: id, ADIF: r.utf8()}
	case TypeSpectrumSummary:
		m = SpectrumSummary{ID: id, PeriodStart: r.qtime(), TRPeriodMs: r.u32(), StartHz: r.u32(),
			BinWidthHz: r.u32(), FloorDB: r.f64(), StepDB: r.f64(), Levels: r.byteArray()}
	default:
		return nil, fmt.Errorf("%w: %d", ErrUnknownType, typ)
	}
	if r.err != nil {
		return nil, fmt.Errorf("type %d: %w", typ, r.err)
	}
	return m, nil
}

func (r *reader) status(id string) Status {
	// Go evaluates the fields of a composite literal from left to right,
	// so the reads follow the wire order.
	return Status{ID: id, DialHz: r.u64(), Mode: r.utf8(), DXCall: r.utf8(), Report: r.utf8(),
		TxMode: r.utf8(), TxEnabled: r.boolean(), Transmitting: r.boolean(), Decoding: r.boolean(),
		RxDF: r.u32(), TxDF: r.u32(), DECall: r.utf8(), DEGrid: r.utf8(), DXGrid: r.utf8(),
		TxWatchdog: r.boolean(), SubMode: r.utf8(), FastMode: r.boolean(), SpecialOp: SpecialOp(r.u8()),
		FreqTolerance: r.u32(), TRPeriod: r.u32(), ConfigName: r.utf8(), TxMessage: r.utf8()}
}

func (r *reader) qsoLogged(id string) QSOLogged {
	return QSOLogged{ID: id, TimeOff: r.qdatetime(), DXCall: r.utf8(), DXGrid: r.utf8(), TxFreqHz: r.u64(),
		Mode: r.utf8(), ReportSent: r.utf8(), ReportReceived: r.utf8(), TxPower: r.utf8(),
		Comments: r.utf8(), Name: r.utf8(), TimeOn: r.qdatetime(), OperatorCall: r.utf8(),
		MyCall: r.utf8(), MyGrid: r.utf8(), ExchangeSent: r.utf8(), ExchangeRcvd: r.utf8(), PropMode: r.utf8()}
}

// reader reads QDataStream fields in big-endian order. After the first
// error it keeps the error and returns zero values.
type reader struct {
	b   []byte
	off int
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.b)-r.off {
		r.err = ErrShort
		return nil
	}
	p := r.b[r.off : r.off+n]
	r.off += n
	return p
}

func (r *reader) u8() uint8 {
	if p := r.take(1); p != nil {
		return p[0]
	}
	return 0
}

func (r *reader) boolean() bool { return r.u8() != 0 }

func (r *reader) u32() uint32 {
	if p := r.take(4); p != nil {
		return binary.BigEndian.Uint32(p)
	}
	return 0
}

func (r *reader) i32() int32 { return int32(r.u32()) }

func (r *reader) u64() uint64 {
	if p := r.take(8); p != nil {
		return binary.BigEndian.Uint64(p)
	}
	return 0
}

func (r *reader) i64() int64 { return int64(r.u64()) }

func (r *reader) f64() float64 { return math.Float64frombits(r.u64()) }

// nullLength is the length field of a null QByteArray.
const nullLength = math.MaxUint32

// byteArray reads a QByteArray. A null array gives nil.
func (r *reader) byteArray() []byte {
	n := r.u32()
	if r.err != nil || n == nullLength {
		return nil
	}
	if uint64(n) > uint64(len(r.b)-r.off) {
		r.err = ErrShort
		return nil
	}
	return append([]byte(nil), r.take(int(n))...)
}

// utf8 reads a utf8 field. A null string gives "".
func (r *reader) utf8() string { return string(r.byteArray()) }

// nullTime is the value of a null QTime.
const nullTime = math.MaxUint32

// qtime reads a QTime as a duration since midnight. A null time gives -1.
func (r *reader) qtime() time.Duration {
	ms := r.u32()
	if ms == nullTime {
		return -1
	}
	return time.Duration(ms) * time.Millisecond
}

// julianDayUnixEpoch is the Julian day number of 1970-01-01.
const julianDayUnixEpoch = 2440588

// Time specs of a QDateTime that carry extra fields. Spec 0 is local time
// and spec 1 is UTC.
const (
	specOffset   = 2
	specTimeZone = 3
)

// qdatetime reads a QDateTime in the Qt 5.4 format. WSJT-X sends UTC. The
// decoder reads local time as UTC, and it reads a time zone as UTC when Go
// does not know the zone. A null date gives the zero time.
func (r *reader) qdatetime() time.Time {
	jd := r.i64()
	tod := r.qtime()
	spec := r.u8()
	loc := time.UTC
	switch spec {
	case specOffset:
		loc = time.FixedZone("", int(r.i32()))
	case specTimeZone:
		if z, err := time.LoadLocation(r.utf8()); err == nil {
			loc = z
		}
	}
	if r.err != nil || jd == math.MinInt64 {
		return time.Time{}
	}
	if tod < 0 {
		tod = 0
	}
	t := time.Date(1970, 1, 1, 0, 0, 0, 0, loc).AddDate(0, 0, int(jd-julianDayUnixEpoch))
	return t.Add(tod)
}
