// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// t0 is the start of an FT8 slot. Every harness clock starts here.
var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// Test identities. All calls and grids are invented.
const (
	ownHost    = "op-pc"
	laptopHost = "op-laptop"
	ownCall    = "XX0OP"
	ownGrid    = "JJ00"
	wsjtxID    = "WSJT-X"
	fullRev    = "abc123-hunter"
	stockRev   = "abc123"
)

var peerAddr = netip.MustParseAddrPort("127.0.0.1:52001")

// cmd is one command that the gate sent, read with an independent
// parser of the wire format.
type cmd struct {
	Type uint32
	ID   string
	// Flag is the bool of HaltTx, SetHoldTxFreq, SetTxEnabled and
	// FreeText.
	Flag bool
	// Num is the offset of SetTxDF, the delta frequency of Reply and the
	// dial of SetDialFrequency.
	Num uint64
	// Text is the message of Reply, the text of FreeText and the mode of
	// Configure.
	Text string
}

func (c cmd) String() string {
	return fmt.Sprintf("{type %d flag %v num %d text %q}", c.Type, c.Flag, c.Num, c.Text)
}

// recConn records every datagram.
type recConn struct {
	t    *testing.T
	cmds []cmd
	// onWrite runs for each command as it goes out.
	onWrite func(cmd)
	// fail makes the write of a command fail when it returns true. The
	// command still goes into cmds, so a test sees each attempt.
	fail func(cmd) bool
}

// errWrite is the error of a write that fail makes fail.
var errWrite = errors.New("test: write failed")

// failType makes each write of a command of type typ fail.
func failType(typ uint32) func(cmd) bool {
	return func(c cmd) bool { return c.Type == typ }
}

func (r *recConn) WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error) {
	if addr != peerAddr {
		r.t.Errorf("datagram to %v, want %v", addr, peerAddr)
	}
	c := parseCmd(r.t, b)
	if r.onWrite != nil {
		r.onWrite(c)
	}
	r.cmds = append(r.cmds, c)
	if r.fail != nil && r.fail(c) {
		return 0, errWrite
	}
	return len(b), nil
}

// parseCmd reads a command. It does not use the encoders of package send.
func parseCmd(t *testing.T, b []byte) cmd {
	t.Helper()
	off := 0
	u32 := func() uint32 {
		if off+4 > len(b) {
			t.Fatalf("short command %x", b)
		}
		v := binary.BigEndian.Uint32(b[off:])
		off += 4
		return v
	}
	str := func() string {
		n := int(u32())
		if n == -1 || uint32(n) == 0xffffffff {
			return ""
		}
		s := string(b[off : off+n])
		off += n
		return s
	}
	boolean := func() bool {
		v := b[off] != 0
		off++
		return v
	}
	if u32() != 0xadbccbda || u32() != 3 {
		t.Fatalf("bad header %x", b)
	}
	c := cmd{Type: u32(), ID: str()}
	switch c.Type {
	case wsjtx.TypeHaltTx, wsjtx.TypeSetHoldTxFreq, wsjtx.TypeSetTxEnabled:
		c.Flag = boolean()
	case wsjtx.TypeSetTxDF:
		c.Num = uint64(u32())
	case wsjtx.TypeFreeText:
		c.Text = str()
		c.Flag = boolean()
	case wsjtx.TypeSetDialFrequency:
		if off+8 > len(b) {
			t.Fatalf("short command %x", b)
		}
		c.Num = binary.BigEndian.Uint64(b[off:])
	case wsjtx.TypeConfigure:
		c.Text = str()
	case wsjtx.TypeReply:
		off += 4 + 4 + 8
		c.Num = uint64(u32())
		_ = str()
		c.Text = str()
	}
	return c
}

// harness drives one core with a fake clock. Every input goes in on the
// test goroutine, so the tests need no locks and no real waits.
type harness struct {
	t        *testing.T
	cfg      config.Config
	now      time.Time
	conn     *recConn
	c        *core
	pubs     []published
	task     string
	listsErr error
	resets   int
	// skip names the inputs that refresh does not send.
	skip map[string]bool
	// status is the Status that refresh sends.
	status wsjtx.Status
	build  string
	// link is the number of the broker connection in each bus message.
	link uint64
	// noPeer makes the gate know no WSJT-X address.
	noPeer bool
}

type published struct {
	State bus.State
	At    time.Time
}

// newHarness builds a core with the default limits. mod can change the
// configuration.
func newHarness(t *testing.T, mod ...func(*config.Config)) *harness {
	t.Helper()
	cfg := config.Defaults()
	cfg.Host = ownHost
	cfg.Presence.Hosts = []string{ownHost, laptopHost}
	cfg.WSJTX.ID = wsjtxID
	cfg.Bands = []config.Band{
		{Name: "40m", Modes: []string{"FT8"}},
		{Name: "20m", Modes: []string{"FT8", "FT4"}},
		{Name: "17m", Modes: []string{"FT8", "FT4"}},
	}
	for _, m := range mod {
		m(&cfg)
	}
	h := &harness{t: t, cfg: cfg, now: t0, skip: map[string]bool{}, build: fullRev, link: 1}
	h.conn = &recConn{t: t}
	h.status = wsjtx.Status{ID: wsjtxID, DialHz: 14_074_000, Mode: "FT8", TxDF: 1500, DECall: ownCall, DEGrid: ownGrid, TRPeriod: 15}
	h.c = newCore(coreOptions{
		Config:    cfg,
		Conn:      h.conn,
		Peer:      func() (netip.AddrPort, bool) { return peerAddr, !h.noPeer },
		ResetPeer: func() { h.resets++ },
		Now:       func() time.Time { return h.now },
		Publish:   func(s bus.State) { h.pubs = append(h.pubs, published{s, h.now}) },
		Logger:    slog.New(slog.DiscardHandler),
		Task:      func() string { return h.task },
		ListsErr:  func() error { return h.listsErr },
	})
	return h
}

// stock makes the harness report a stock WSJT-X build.
func (h *harness) stock() *harness { h.build = stockRev; return h }

func (h *harness) meta(topic string) bus.Meta {
	return bus.Meta{Topic: topic, Received: h.now, TS: h.now, Conn: h.link}
}

func (h *harness) bus(e bus.Event) { h.c.handleBus(e) }

func (h *harness) wsjtx(m wsjtx.Message) {
	h.c.handleWSJTX(wsjtx.Event{Kind: wsjtx.EventMessage, Msg: m, From: peerAddr, At: h.now})
}

func (h *harness) sendStatus(mod func(*wsjtx.Status)) {
	s := h.status
	if mod != nil {
		mod(&s)
	}
	h.wsjtx(s)
}

// refresh sends a fresh copy of each input that skip does not name.
func (h *harness) refresh() {
	if !h.skip["heartbeat"] {
		h.wsjtx(wsjtx.Heartbeat{ID: wsjtxID, MaxSchema: 3, Version: "2.7.0", Revision: h.build})
	}
	if !h.skip["presence"] {
		h.bus(bus.Presence{Meta: h.meta("site/hf/logotter/presence"), Present: true, Host: ownHost})
	}
	if !h.skip["chase"] {
		h.bus(bus.ChaseState{Meta: h.meta("site/hf/chase/state"), Seq: 1, Activity: "dx"})
	}
}

// live brings up the link and every input.
func (h *harness) live() *harness {
	h.bus(bus.Link{Up: true, Conn: h.link, At: h.now})
	h.refresh()
	h.sendStatus(nil)
	return h
}

// enable enables the gate and fails the test on a refusal.
func (h *harness) enable() *harness {
	h.t.Helper()
	if err := h.c.enable(); err != nil {
		h.t.Fatalf("enable: %v", err)
	}
	if a := h.c.snapshot().Arm; a != Running {
		h.t.Fatalf("arm %v after enable, want running", a)
	}
	return h
}

// advance moves the clock by d without new inputs and lets the gate run
// its timers.
func (h *harness) advance(d time.Duration) {
	h.now = h.now.Add(d)
	h.c.tick()
}

// pass moves the clock by d in steps of 1 s. It refreshes the inputs
// every 5 s, except the inputs in skip.
func (h *harness) pass(d time.Duration) {
	end := h.now.Add(d)
	for n := 1; h.now.Before(end); n++ {
		h.advance(min(time.Second, end.Sub(h.now)))
		if n%5 == 0 {
			h.refresh()
		}
	}
}

// take returns the commands sent since the last take.
func (h *harness) take() []cmd {
	c := h.conn.cmds
	h.conn.cmds = nil
	return c
}

// halts counts HaltTx commands by their auto only flag.
func halts(cmds []cmd) (now, autoOnly int) {
	for _, c := range cmds {
		if c.Type == wsjtx.TypeHaltTx {
			if c.Flag {
				autoOnly++
			} else {
				now++
			}
		}
	}
	return now, autoOnly
}

func countType(cmds []cmd, typ uint32) int {
	n := 0
	for _, c := range cmds {
		if c.Type == typ {
			n++
		}
	}
	return n
}

// wantStopped fails unless the gate is stopped with reason.
func (h *harness) wantStopped(reason string) {
	h.t.Helper()
	s := h.c.snapshot()
	if s.Arm != Stopped || s.StopReason != reason {
		h.t.Fatalf("arm %v reason %q, want stopped with %q", s.Arm, s.StopReason, reason)
	}
}

func (h *harness) wantRunning() {
	h.t.Helper()
	if s := h.c.snapshot(); s.Arm != Running {
		h.t.Fatalf("arm %v reason %q, want running", s.Arm, s.StopReason)
	}
}

// cqDecode is a CQ decode of the station XX1AA in the slot that starts
// at slot, at offset df.
func cqDecode(slot time.Time, df uint32) wsjtx.Decode {
	tod := slot.Sub(time.Date(slot.Year(), slot.Month(), slot.Day(), 0, 0, 0, 0, time.UTC))
	return wsjtx.Decode{ID: wsjtxID, New: true, Time: tod, SNR: -10, DeltaTime: 0.1, DeltaFreq: df, Mode: "~", Message: "CQ XX1AA KK11"}
}

// callerDecode is a decode of XY3CC that calls the operator.
func callerDecode(slot time.Time, df uint32) wsjtx.Decode {
	d := cqDecode(slot, df)
	d.Message = ownCall + " XY3CC KK11"
	return d
}

func wantDecision(t *testing.T, d Decision, ok bool, reason string) {
	t.Helper()
	if d.OK != ok || d.Reason != reason {
		t.Fatalf("decision %+v, want ok %v reason %q", d, ok, reason)
	}
}
