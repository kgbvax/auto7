// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// lockedConn records datagrams from the gate goroutine.
type lockedConn struct {
	t    *testing.T
	mu   sync.Mutex
	cmds []cmd
}

func (l *lockedConn) WriteToUDPAddrPort(b []byte, _ netip.AddrPort) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cmds = append(l.cmds, parseCmd(l.t, b))
	return len(b), nil
}

func (l *lockedConn) take() []cmd {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.cmds
	l.cmds = nil
	return c
}

// chanPublisher hands each published state to a channel.
type chanPublisher chan bus.State

func (p chanPublisher) PublishState(_ context.Context, s bus.State) error {
	p <- s
	return nil
}

// waitState polls the state of g until ok gives true. The gate handles
// a channel input after the send returns, so a test waits for it here.
func waitState(t *testing.T, g *Gate, ok func(State) bool) {
	t.Helper()
	for range 2000 {
		if ok(g.State()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("state %+v did not come", g.State())
}

// waitPub reads published states until ok gives true.
func waitPub(t *testing.T, pubs chanPublisher, ok func(bus.State) bool) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case s := <-pubs:
			if ok(s) {
				return
			}
		case <-timeout:
			t.Fatal("state not published")
		}
	}
}

// TestGateRunsOnOneGoroutine drives the gate through its channels and
// methods, as cmd/auto7 does.
func TestGateRunsOnOneGoroutine(t *testing.T) {
	var clock atomic.Int64
	clock.Store(t0.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	cfg := config.Defaults()
	cfg.Host, cfg.Presence.Hosts, cfg.WSJTX.ID = ownHost, []string{ownHost}, wsjtxID
	conn := &lockedConn{t: t}
	wsjtxCh, busCh, ticks := make(chan wsjtx.Event), make(chan bus.Event), make(chan time.Time)
	pubs := make(chanPublisher, 16)
	g := New(Options{
		Config:    cfg,
		Conn:      conn,
		Peer:      func() (netip.AddrPort, bool) { return peerAddr, true },
		Publisher: pubs,
		WSJTX:     wsjtxCh,
		Bus:       busCh,
		Now:       now,
		Ticks:     ticks,
		Logger:    slog.New(slog.DiscardHandler),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- g.Run(ctx) }()

	if s := g.State(); s.Arm != Stopped {
		t.Fatalf("arm %v at start", s.Arm)
	}
	meta := bus.Meta{Received: now(), TS: now(), Conn: 1}
	// The channels have no buffer, so each event is in the gate before
	// the next call.
	busCh <- bus.Link{Up: true, Conn: 1}
	busCh <- bus.Presence{Meta: meta, Present: true, Host: ownHost}
	busCh <- bus.ChaseState{Meta: meta}
	wsjtxCh <- wsjtx.Event{Kind: wsjtx.EventPeerFixed, Msg: wsjtx.Heartbeat{ID: wsjtxID, Revision: fullRev}, At: now()}
	wsjtxCh <- wsjtx.Event{Msg: wsjtx.Status{ID: wsjtxID, Mode: "FT8", TxDF: 1500}, At: now()}
	if err := g.Enable(ctx); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if s := g.State(); s.Arm != Running || s.PresenceHost != ownHost {
		t.Fatalf("state %+v after enable", s)
	}
	// The publisher goroutine publishes the enabled state.
	waitPub(t, pubs, func(s bus.State) bool { return s.Enabled })
	clock.Store(t0.Add(13 * time.Second).UnixNano())
	d := g.Submit(ctx, Call{Decode: cqDecode(t0, 800), Heard: now()})
	wantDecision(t, d, true, "")
	c := conn.take()
	if len(c) != 2 || c[0].Type != wsjtx.TypeSetHoldTxFreq || c[1].Type != wsjtx.TypeReply {
		t.Fatalf("sent %v, want SetHoldTxFreq and Reply", c)
	}
	// Presence lapses. The tick runs the check.
	clock.Store(t0.Add(91 * time.Second).UnixNano())
	ticks <- now()
	waitState(t, g, func(s State) bool { return s.Arm == Stopped && s.StopReason == ReasonNoPresence })
	if n, _ := halts(conn.take()); n != 1 {
		t.Fatal("no HaltTx at once")
	}
	waitPub(t, pubs, func(s bus.State) bool { return !s.Enabled && s.StopReason == ReasonNoPresence })
	// Enable again, then end Run: the gate sends HaltTx before it exits.
	clock.Store(t0.Add(92 * time.Second).UnixNano())
	meta = bus.Meta{Received: now(), TS: now(), Conn: 1}
	busCh <- bus.Presence{Meta: meta, Present: true, Host: ownHost}
	wsjtxCh <- wsjtx.Event{Msg: wsjtx.Heartbeat{ID: wsjtxID, Revision: fullRev}, At: now()}
	if err := g.Enable(ctx); err != nil {
		t.Fatalf("enable: %v", err)
	}
	conn.take()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n, _ := halts(conn.take()); n != 1 {
		t.Fatal("no HaltTx at exit")
	}
	if s := g.State(); s.StopReason != ReasonExit {
		t.Fatalf("reason %q at exit", s.StopReason)
	}
	if err := g.Enable(context.Background()); err != ErrClosed {
		t.Fatalf("enable after Run: %v, want ErrClosed", err)
	}
}

func TestGateBusChannelClosedStops(t *testing.T) {
	var clock atomic.Int64
	clock.Store(t0.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	cfg := config.Defaults()
	cfg.Host, cfg.Presence.Hosts, cfg.WSJTX.ID = ownHost, []string{ownHost}, wsjtxID
	conn := &lockedConn{t: t}
	wsjtxCh, busCh := make(chan wsjtx.Event), make(chan bus.Event)
	g := New(Options{Config: cfg, Conn: conn, Peer: func() (netip.AddrPort, bool) { return peerAddr, true },
		WSJTX: wsjtxCh, Bus: busCh, Now: now, Ticks: make(chan time.Time), Logger: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)
	meta := bus.Meta{Received: now(), TS: now(), Conn: 1}
	busCh <- bus.Link{Up: true, Conn: 1}
	busCh <- bus.Presence{Meta: meta, Present: true, Host: ownHost}
	wsjtxCh <- wsjtx.Event{Msg: wsjtx.Heartbeat{ID: wsjtxID, Revision: stockRev}, At: now()}
	if err := g.Enable(ctx); err != nil {
		t.Fatal(err)
	}
	close(busCh)
	waitState(t, g, func(s State) bool { return s.Arm == Stopped && s.StopReason == bus.BusDown })
	if err := g.Enable(ctx); err == nil || err.Error() != bus.BusDown {
		t.Fatalf("enable after the bus closed: %v", err)
	}
}
