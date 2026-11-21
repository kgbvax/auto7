// SPDX-License-Identifier: AGPL-3.0-or-later

package peers

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"
	"time"
)

// This file carries its own small encoder and decoder of the WSJT-X UDP
// protocol on purpose. It shares no code with the auto7 codec, so an error
// in that codec cannot hide in both sides of a test.

// Magic is the first quint32 of every WSJT-X datagram.
const Magic uint32 = 0xadbccbda

// Schema is the schema number that the fake sends.
const Schema uint32 = 3

// Message types of the WSJT-X protocol and of ws-hunter.
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
	TypeConfigure        uint32 = 15
	TypeSetTxPeriod      uint32 = 19
	TypeSetDialFrequency uint32 = 20
	TypeSetTxDF          uint32 = 24
	TypeSetHoldTxFreq    uint32 = 25
	TypeSetTxEnabled     uint32 = 26
	TypeSpectrumSummary  uint32 = 27
	TypeClearDX          uint32 = 28
)

// Special operation modes of the Status message.
const (
	SpecialOpNone  uint8 = 0
	SpecialOpFox   uint8 = 6
	SpecialOpHound uint8 = 7
)

// RevisionStock and RevisionHunter are Heartbeat revisions of a stock build
// and of a ws-hunter build.
const (
	RevisionStock  = "a1b2c3"
	RevisionHunter = "a1b2c3-hunter"
)

// Status is the scripted content of a Status message.
type Status struct {
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
	SpecialOp    uint8
	// FreqTolerance and TRPeriod are math.MaxUint32 when not applicable.
	FreqTolerance uint32
	TRPeriod      uint32
	ConfigName    string
	TxMessage     string
}

// Decode is the scripted content of a Decode message.
type Decode struct {
	New bool
	// Time is the start of the period. Only the time of day in UTC counts.
	Time      time.Time
	SNR       int32
	DeltaTime float64
	DeltaFreq uint32
	Mode      string
	Message   string
	LowConf   bool
	OffAir    bool
}

// QSOLogged is the scripted content of a QSO Logged message.
type QSOLogged struct {
	TimeOff      time.Time
	DXCall       string
	DXGrid       string
	TxFreqHz     uint64
	Mode         string
	ReportSent   string
	ReportRcvd   string
	TxPower      string
	Comments     string
	Name         string
	TimeOn       time.Time
	OperatorCall string
	MyCall       string
	MyGrid       string
	ExchangeSent string
	ExchangeRcvd string
	PropMode     string
}

// Command is one inbound datagram that the fake received.
type Command struct {
	Type uint32
	ID   string
	// Flag is the bool field of HaltTx (Auto Tx Only), SetTxPeriod (Tx
	// First), SetHoldTxFreq (Hold), SetTxEnabled (Enabled) and FreeText
	// (Send).
	Flag bool
	// Number is the frequency of SetDialFrequency, the offset of SetTxDF
	// and the Delta frequency of Reply, in Hz.
	Number uint64
	// Text is the Message of Reply, the Text of FreeText and the Mode of
	// Configure.
	Text string
	// Body holds the bytes after the Id field.
	Body []byte
	// Err is set when the fake could not read the fields of a known type.
	Err error
	// At is the local receive time of the datagram.
	At time.Time
}

// WSJTX is a fake WSJT-X instance. It sends scripted datagrams from a
// loopback UDP socket to the target address and records every datagram
// that comes back to that socket.
type WSJTX struct {
	id     string
	conn   *net.UDPConn
	target *net.UDPAddr

	mu      sync.Mutex
	cmds    []Command
	changed chan struct{}
	done    chan struct{}
}

// NewWSJTX opens a socket on a free loopback port. The fake sends as the
// instance id to target, a host:port address.
func NewWSJTX(id, target string) (*WSJTX, error) {
	to, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	w := &WSJTX{id: id, conn: conn, target: to, changed: make(chan struct{}), done: make(chan struct{})}
	go w.read()
	return w, nil
}

// Addr is the address of the fake, from which it sends.
func (w *WSJTX) Addr() *net.UDPAddr { return w.conn.LocalAddr().(*net.UDPAddr) }

// Close closes the socket.
func (w *WSJTX) Close() error {
	err := w.conn.Close()
	<-w.done
	return err
}

// SendHeartbeat sends a Heartbeat with the revision, for example
// RevisionStock or RevisionHunter.
func (w *WSJTX) SendHeartbeat(revision string) error {
	e := w.header(TypeHeartbeat)
	e.u32(Schema)
	e.utf8("2.7.0")
	e.utf8(revision)
	return w.send(e)
}

// SendStatus sends a Status message.
func (w *WSJTX) SendStatus(s Status) error {
	e := w.header(TypeStatus)
	e.u64(s.DialHz)
	e.utf8(s.Mode)
	e.utf8(s.DXCall)
	e.utf8(s.Report)
	e.utf8(s.TxMode)
	e.boolean(s.TxEnabled)
	e.boolean(s.Transmitting)
	e.boolean(s.Decoding)
	e.u32(s.RxDF)
	e.u32(s.TxDF)
	e.utf8(s.DECall)
	e.utf8(s.DEGrid)
	e.utf8(s.DXGrid)
	e.boolean(s.TxWatchdog)
	e.utf8(s.SubMode)
	e.boolean(s.FastMode)
	e.u8(s.SpecialOp)
	e.u32(s.FreqTolerance)
	e.u32(s.TRPeriod)
	e.utf8(s.ConfigName)
	e.utf8(s.TxMessage)
	return w.send(e)
}

// SendDecode sends a Decode message.
func (w *WSJTX) SendDecode(d Decode) error {
	e := w.header(TypeDecode)
	e.boolean(d.New)
	e.qtime(d.Time)
	e.i32(d.SNR)
	e.f64(d.DeltaTime)
	e.u32(d.DeltaFreq)
	e.utf8(d.Mode)
	e.utf8(d.Message)
	e.boolean(d.LowConf)
	e.boolean(d.OffAir)
	return w.send(e)
}

// SendQSOLogged sends a QSO Logged message.
func (w *WSJTX) SendQSOLogged(q QSOLogged) error {
	e := w.header(TypeQSOLogged)
	e.qdatetime(q.TimeOff)
	e.utf8(q.DXCall)
	e.utf8(q.DXGrid)
	e.u64(q.TxFreqHz)
	e.utf8(q.Mode)
	e.utf8(q.ReportSent)
	e.utf8(q.ReportRcvd)
	e.utf8(q.TxPower)
	e.utf8(q.Comments)
	e.utf8(q.Name)
	e.qdatetime(q.TimeOn)
	e.utf8(q.OperatorCall)
	e.utf8(q.MyCall)
	e.utf8(q.MyGrid)
	e.utf8(q.ExchangeSent)
	e.utf8(q.ExchangeRcvd)
	e.utf8(q.PropMode)
	return w.send(e)
}

// Commands returns a copy of every received command, oldest first.
func (w *WSJTX) Commands() []Command {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Command(nil), w.cmds...)
}

// WaitCommand waits until a command of the type arrives and returns the
// first one. It gives an error after the timeout.
func (w *WSJTX) WaitCommand(typ uint32, timeout time.Duration) (Command, error) {
	deadline := time.After(timeout)
	for {
		w.mu.Lock()
		for _, c := range w.cmds {
			if c.Type == typ {
				w.mu.Unlock()
				return c, nil
			}
		}
		changed := w.changed
		w.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			return Command{}, fmt.Errorf("no command of type %d within %s", typ, timeout)
		}
	}
}

func (w *WSJTX) read() {
	defer close(w.done)
	buf := make([]byte, 65536)
	for {
		n, _, err := w.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		cmd, ok := parseCommand(buf[:n])
		if !ok {
			continue
		}
		cmd.At = time.Now()
		w.mu.Lock()
		w.cmds = append(w.cmds, cmd)
		close(w.changed)
		w.changed = make(chan struct{})
		w.mu.Unlock()
	}
}

func (w *WSJTX) header(typ uint32) *encoder {
	e := &encoder{}
	e.u32(Magic)
	e.u32(Schema)
	e.u32(typ)
	e.utf8(w.id)
	return e
}

func (w *WSJTX) send(e *encoder) error {
	_, err := w.conn.WriteToUDP(e.buf.Bytes(), w.target)
	return err
}

// parseCommand reads the header and the fields of the known command
// types. It returns false when the datagram has no valid header.
func parseCommand(b []byte) (Command, bool) {
	d := &decoder{b: b}
	if d.u32() != Magic {
		return Command{}, false
	}
	_ = d.u32() // schema
	c := Command{Type: d.u32(), ID: d.utf8()}
	if d.err != nil {
		return Command{}, false
	}
	c.Body = append([]byte(nil), d.b[d.off:]...)
	switch c.Type {
	case TypeHaltTx, TypeSetTxPeriod, TypeSetHoldTxFreq, TypeSetTxEnabled:
		c.Flag = d.boolean()
	case TypeSetDialFrequency:
		c.Number = d.u64()
	case TypeSetTxDF:
		c.Number = uint64(d.u32())
	case TypeFreeText:
		c.Text = d.utf8()
		c.Flag = d.boolean()
	case TypeConfigure:
		c.Text = d.utf8()
	case TypeReply:
		_ = d.u32() // time
		_ = d.u32() // snr
		_ = d.u64() // delta time
		c.Number = uint64(d.u32())
		_ = d.utf8() // mode
		c.Text = d.utf8()
	}
	c.Err = d.err
	return c, true
}

// encoder writes QDataStream fields in big-endian order.
type encoder struct{ buf bytes.Buffer }

func (e *encoder) u8(v uint8)   { e.buf.WriteByte(v) }
func (e *encoder) u32(v uint32) { e.buf.Write(binary.BigEndian.AppendUint32(nil, v)) }
func (e *encoder) i32(v int32)  { e.u32(uint32(v)) }
func (e *encoder) u64(v uint64) { e.buf.Write(binary.BigEndian.AppendUint64(nil, v)) }
func (e *encoder) f64(v float64) {
	e.u64(math.Float64bits(v))
}
func (e *encoder) boolean(v bool) {
	if v {
		e.u8(1)
	} else {
		e.u8(0)
	}
}

// utf8 writes a QByteArray: a quint32 length and the bytes.
func (e *encoder) utf8(s string) {
	e.u32(uint32(len(s)))
	e.buf.WriteString(s)
}

// qtime writes the milliseconds since midnight UTC.
func (e *encoder) qtime(t time.Time) {
	t = t.UTC()
	ms := ((t.Hour()*60+t.Minute())*60+t.Second())*1000 + t.Nanosecond()/1e6
	e.u32(uint32(ms))
}

// julianDayUnixEpoch is the Julian day number of 1970-01-01.
const julianDayUnixEpoch = 2440588

// qdatetime writes a QDateTime in UTC: Julian day, QTime and timespec 1.
func (e *encoder) qdatetime(t time.Time) {
	t = t.UTC()
	days := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400
	e.u64(uint64(days + julianDayUnixEpoch))
	e.qtime(t)
	e.u8(1)
}

// decoder reads QDataStream fields. After the first short read it keeps
// err and returns zero values.
type decoder struct {
	b   []byte
	off int
	err error
}

var errShort = errors.New("datagram too short")

func (d *decoder) take(n int) []byte {
	if d.err != nil || d.off+n > len(d.b) {
		d.err = errShort
		return nil
	}
	p := d.b[d.off : d.off+n]
	d.off += n
	return p
}

func (d *decoder) u32() uint32 {
	if p := d.take(4); p != nil {
		return binary.BigEndian.Uint32(p)
	}
	return 0
}

func (d *decoder) u64() uint64 {
	if p := d.take(8); p != nil {
		return binary.BigEndian.Uint64(p)
	}
	return 0
}

func (d *decoder) boolean() bool {
	if p := d.take(1); p != nil {
		return p[0] != 0
	}
	return false
}

func (d *decoder) utf8() string {
	n := d.u32()
	if n == math.MaxUint32 {
		return ""
	}
	return string(d.take(int(n)))
}
