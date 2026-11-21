// SPDX-License-Identifier: AGPL-3.0-or-later

// Package send holds the command encoders and the UDP sender for WSJT-X
// and ws-hunter. Only the TX gate uses this package. A test in
// internal/tx fails when a package outside internal/tx imports it.
//
// Every command has the WSJT-X framing: the magic number, the schema, the
// type and the Id, then the fields in the big-endian QDataStream format
// of Qt 5.
package send

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/netip"

	"github.com/kgbvax/auto7/internal/wsjtx"
)

// MaxDatagram is the largest command that the sender sends. ws-hunter
// reads at most this many bytes.
const MaxDatagram = 4096

// NoChange is the quint32 value that tells WSJT-X not to change a
// numeric field of Configure.
const NoChange = math.MaxUint32

// ClearAll is the callsign of HighlightCallsign that clears all
// highlights in WSJT-X.
const ClearAll = "CLEARALL!"

// Errors of the sender.
var (
	// ErrNoPeer means that no WSJT-X address is known yet.
	ErrNoPeer = errors.New("send: no WSJT-X address known")
	// ErrTooLong means that the command is longer than MaxDatagram.
	ErrTooLong = errors.New("send: command longer than 4096 bytes")
)

// writer appends QDataStream fields in big-endian order.
type writer struct{ b []byte }

// header starts a command with the magic, schema 3, the type and the Id.
func header(typ uint32, id string) *writer {
	w := &writer{b: make([]byte, 0, 64)}
	w.u32(wsjtx.Magic)
	w.u32(wsjtx.Schema)
	w.u32(typ)
	w.utf8(id)
	return w
}

func (w *writer) u8(v uint8)    { w.b = append(w.b, v) }
func (w *writer) u16(v uint16)  { w.b = binary.BigEndian.AppendUint16(w.b, v) }
func (w *writer) u32(v uint32)  { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *writer) i32(v int32)   { w.u32(uint32(v)) }
func (w *writer) u64(v uint64)  { w.b = binary.BigEndian.AppendUint64(w.b, v) }
func (w *writer) f64(v float64) { w.u64(math.Float64bits(v)) }

func (w *writer) boolean(v bool) {
	if v {
		w.u8(1)
		return
	}
	w.u8(0)
}

// utf8 writes a QByteArray with the UTF-8 bytes of s. An empty string is
// an empty array, not a null array.
func (w *writer) utf8(s string) {
	w.u32(uint32(len(s)))
	w.b = append(w.b, s...)
}

// qtime writes a QTime as milliseconds since midnight. A negative value
// writes the null time.
func (w *writer) qtime(ms int64) {
	if ms < 0 {
		w.u32(math.MaxUint32)
		return
	}
	w.u32(uint32(ms))
}

// Reply is message type 4. It repeats a decode exactly, as WSJT-X sent
// it. WSJT-X then acts as if the operator double-clicked the decode.
// modifiers holds the keyboard modifier bits of the protocol. 0 means no
// modifier.
func Reply(id string, d wsjtx.Decode, modifiers uint8) []byte {
	w := header(wsjtx.TypeReply, id)
	if d.Time < 0 {
		w.qtime(-1)
	} else {
		w.qtime(d.Time.Milliseconds())
	}
	w.i32(d.SNR)
	w.f64(d.DeltaTime)
	w.u32(d.DeltaFreq)
	w.utf8(d.Mode)
	w.utf8(d.Message)
	w.boolean(d.LowConfidence)
	w.u8(modifiers)
	return w.b
}

// HaltTx is message type 8. With autoOnly true WSJT-X stops at the end of
// the current transmission. With autoOnly false it stops at once.
func HaltTx(id string, autoOnly bool) []byte {
	w := header(wsjtx.TypeHaltTx, id)
	w.boolean(autoOnly)
	return w.b
}

// FreeText is message type 9. It sets the free text message. With send
// true WSJT-X also selects the free text as the next message.
func FreeText(id, text string, send bool) []byte {
	w := header(wsjtx.TypeFreeText, id)
	w.utf8(text)
	w.boolean(send)
	return w.b
}

// Configuration holds the fields of Configure. An empty string and the
// value NoChange leave the field as it is in WSJT-X.
type Configuration struct {
	Mode          string
	FreqTolerance uint32
	SubMode       string
	FastMode      bool
	TRPeriod      uint32
	RxDF          uint32
	DXCall        string
	DXGrid        string
	Generate      bool
}

// ModeOnly gives a Configuration that changes only the mode.
func ModeOnly(mode string) Configuration {
	return Configuration{Mode: mode, FreqTolerance: NoChange, TRPeriod: NoChange, RxDF: NoChange}
}

// Configure is message type 15.
func Configure(id string, c Configuration) []byte {
	w := header(wsjtx.TypeConfigure, id)
	w.utf8(c.Mode)
	w.u32(c.FreqTolerance)
	w.utf8(c.SubMode)
	w.boolean(c.FastMode)
	w.u32(c.TRPeriod)
	w.u32(c.RxDF)
	w.utf8(c.DXCall)
	w.utf8(c.DXGrid)
	w.boolean(c.Generate)
	return w.b
}

// Color is a QColor. The zero value is an invalid color. WSJT-X clears
// the highlight of a call when a color is invalid.
type Color struct {
	Valid   bool
	R, G, B uint8
}

// RGB gives a valid color.
func RGB(r, g, b uint8) Color { return Color{Valid: true, R: r, G: g, B: b} }

// QColor specs of Qt 5.
const (
	specInvalid = 0
	specRGB     = 1
)

// color writes a QColor in the Qt 5 format: the spec as qint8, then
// alpha, red, green, blue and pad as quint16.
func (w *writer) color(c Color) {
	if !c.Valid {
		w.u8(specInvalid)
		w.u16(math.MaxUint16)
		w.u16(0)
		w.u16(0)
		w.u16(0)
		w.u16(0)
		return
	}
	w.u8(specRGB)
	w.u16(math.MaxUint16)
	w.u16(uint16(c.R) * 0x101)
	w.u16(uint16(c.G) * 0x101)
	w.u16(uint16(c.B) * 0x101)
	w.u16(0)
}

// HighlightCallsign is message type 13. With last true WSJT-X highlights
// only the last decode of the call.
func HighlightCallsign(id, call string, bg, fg Color, last bool) []byte {
	w := header(wsjtx.TypeHighlightCall, id)
	w.utf8(call)
	w.color(bg)
	w.color(fg)
	w.boolean(last)
	return w.b
}

// SetDialFrequency is the ws-hunter message type 20. WSJT-X sets the dial
// frequency of the rig by CAT.
func SetDialFrequency(id string, hz uint64) []byte {
	w := header(wsjtx.TypeSetDialFrequency, id)
	w.u64(hz)
	return w.b
}

// SetTxDF is the ws-hunter message type 24. It sets the TX offset in Hz.
func SetTxDF(id string, hz uint32) []byte {
	w := header(wsjtx.TypeSetTxDF, id)
	w.u32(hz)
	return w.b
}

// SetHoldTxFreq is the ws-hunter message type 25.
func SetHoldTxFreq(id string, hold bool) []byte {
	w := header(wsjtx.TypeSetHoldTxFreq, id)
	w.boolean(hold)
	return w.b
}

// SetTxEnabled is the ws-hunter message type 26. It acts like a click on
// Enable Tx.
func SetTxEnabled(id string, enabled bool) []byte {
	w := header(wsjtx.TypeSetTxEnabled, id)
	w.boolean(enabled)
	return w.b
}

// ClearDX is the ws-hunter message type 28. It empties the DX call and
// selects the CQ message as the next message.
func ClearDX(id string) []byte {
	return header(wsjtx.TypeClearDX, id).b
}

// Conn writes one datagram to an address. *net.UDPConn implements it.
type Conn interface {
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
}

// Sender sends commands to the WSJT-X peer. The peer is the address that
// the receiver learned from the WSJT-X traffic.
type Sender struct {
	conn Conn
	id   string
	peer func() (netip.AddrPort, bool)
}

// New builds a sender. id is the WSJT-X instance Id. peer gives the
// current WSJT-X address.
func New(conn Conn, id string, peer func() (netip.AddrPort, bool)) *Sender {
	return &Sender{conn: conn, id: id, peer: peer}
}

// HasPeer reports whether a WSJT-X address is known.
func (s *Sender) HasPeer() bool {
	if s.peer == nil {
		return false
	}
	_, ok := s.peer()
	return ok
}

// write sends one encoded command.
func (s *Sender) write(b []byte) error {
	if len(b) > MaxDatagram {
		return ErrTooLong
	}
	if s.peer == nil || s.conn == nil {
		return ErrNoPeer
	}
	to, ok := s.peer()
	if !ok {
		return ErrNoPeer
	}
	if _, err := s.conn.WriteToUDPAddrPort(b, to); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	return nil
}

// Reply sends Reply with no modifier.
func (s *Sender) Reply(d wsjtx.Decode) error { return s.write(Reply(s.id, d, 0)) }

// HaltTx sends HaltTx.
func (s *Sender) HaltTx(autoOnly bool) error { return s.write(HaltTx(s.id, autoOnly)) }

// FreeText sends FreeText.
func (s *Sender) FreeText(text string, send bool) error {
	return s.write(FreeText(s.id, text, send))
}

// Configure sends Configure.
func (s *Sender) Configure(c Configuration) error { return s.write(Configure(s.id, c)) }

// HighlightCallsign sends HighlightCallsign.
func (s *Sender) HighlightCallsign(call string, bg, fg Color, last bool) error {
	return s.write(HighlightCallsign(s.id, call, bg, fg, last))
}

// SetDialFrequency sends SetDialFrequency.
func (s *Sender) SetDialFrequency(hz uint64) error { return s.write(SetDialFrequency(s.id, hz)) }

// SetTxDF sends SetTxDF.
func (s *Sender) SetTxDF(hz uint32) error { return s.write(SetTxDF(s.id, hz)) }

// SetHoldTxFreq sends SetHoldTxFreq.
func (s *Sender) SetHoldTxFreq(hold bool) error { return s.write(SetHoldTxFreq(s.id, hold)) }

// SetTxEnabled sends SetTxEnabled.
func (s *Sender) SetTxEnabled(enabled bool) error { return s.write(SetTxEnabled(s.id, enabled)) }

// ClearDX sends ClearDX.
func (s *Sender) ClearDX() error { return s.write(ClearDX(s.id)) }
