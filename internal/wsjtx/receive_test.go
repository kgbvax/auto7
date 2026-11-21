// SPDX-License-Identifier: AGPL-3.0-or-later

package wsjtx

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/config"
)

var fixedNow = time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)

type rig struct {
	t      *testing.T
	r      *Receiver
	events chan Event
	done   chan error
}

func startReceiver(t *testing.T) *rig {
	t.Helper()
	r, err := Listen(config.WSJTXConfig{ID: "WSJT-X", Listen: "127.0.0.1:0"}, func() time.Time { return fixedNow })
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	g := &rig{t: t, r: r, events: make(chan Event, 16), done: make(chan error, 1)}
	go func() { g.done <- r.Run(ctx, g.events) }()
	t.Cleanup(func() {
		cancel()
		if err := <-g.done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	return g
}

// client opens a loopback socket that sends to the receiver.
func (g *rig) client() *net.UDPConn {
	g.t.Helper()
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(g.r.Addr()))
	if err != nil {
		g.t.Fatalf("dial: %v", err)
	}
	g.t.Cleanup(func() { c.Close() })
	return c
}

func send(t *testing.T, c *net.UDPConn, b []byte) {
	t.Helper()
	if _, err := c.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func (g *rig) next() Event {
	g.t.Helper()
	select {
	case e := <-g.events:
		return e
	case <-time.After(2 * time.Second):
		g.t.Fatal("no event")
		return Event{}
	}
}

func addrOf(c *net.UDPConn) netip.AddrPort {
	return c.LocalAddr().(*net.UDPAddr).AddrPort()
}

func TestFirstHeartbeatFixesPeer(t *testing.T) {
	g := startReceiver(t)
	if _, ok := g.r.Peer(); ok {
		t.Fatal("peer fixed before any heartbeat")
	}
	c := g.client()
	send(t, c, heartbeat("WSJT-X", "a1b2c3-hunter"))
	e := g.next()
	if e.Kind != EventPeerFixed || e.From != addrOf(c) || !e.At.Equal(fixedNow) {
		t.Fatalf("got %+v", e)
	}
	if hb, ok := e.Msg.(Heartbeat); !ok || hb.Build() != BuildFull {
		t.Fatalf("message: %+v", e.Msg)
	}
	if p, ok := g.r.Peer(); !ok || p != addrOf(c) {
		t.Fatalf("Peer: %v %v, want %v", p, ok, addrOf(c))
	}
	send(t, c, statusDatagram(testStatus).b)
	if e := g.next(); e.Kind != EventMessage || e.Msg != testStatus {
		t.Fatalf("got %+v", e)
	}
}

func TestOtherIDIsDropped(t *testing.T) {
	g := startReceiver(t)
	c := g.client()
	other := testStatus
	other.ID = "WSJT-X - Second"
	send(t, c, statusDatagram(other).b)
	send(t, c, heartbeat("WSJT-X - Second", "a1b2c3"))
	send(t, c, heartbeat("wsjt-x", "a1b2c3"))
	send(t, c, newDatagram(17, "WSJT-X").b) // unknown type
	send(t, c, heartbeat("WSJT-X", "a1b2c3"))
	e := g.next()
	if e.Kind != EventPeerFixed || e.Msg.InstanceID() != "WSJT-X" {
		t.Fatalf("got %+v, want the heartbeat of the configured Id", e)
	}
}

func TestHeartbeatFromNewPortReportsPeerChanged(t *testing.T) {
	g := startReceiver(t)
	a, b := g.client(), g.client()
	send(t, a, heartbeat("WSJT-X", "a1b2c3"))
	if e := g.next(); e.Kind != EventPeerFixed {
		t.Fatalf("got %+v", e)
	}
	send(t, b, heartbeat("WSJT-X", "a1b2c3"))
	e := g.next()
	if e.Kind != EventPeerChanged || e.From != addrOf(b) {
		t.Fatalf("got %+v, want peer changed from %v", e, addrOf(b))
	}
	if p, _ := g.r.Peer(); p != addrOf(a) {
		t.Fatalf("peer moved to %v", p)
	}
	// A Status from the new port is no input either.
	send(t, b, statusDatagram(testStatus).b)
	if e := g.next(); e.Kind != EventPeerChanged {
		t.Fatalf("got %+v", e)
	}
	// After a reset the next heartbeat fixes the peer again.
	g.r.ResetPeer()
	send(t, b, heartbeat("WSJT-X", "a1b2c3"))
	if e := g.next(); e.Kind != EventPeerFixed || e.From != addrOf(b) {
		t.Fatalf("got %+v", e)
	}
	if p, _ := g.r.Peer(); p != addrOf(b) {
		t.Fatalf("peer is %v, want %v", p, addrOf(b))
	}
}

func TestMessageBeforeHeartbeatIsDelivered(t *testing.T) {
	g := startReceiver(t)
	c := g.client()
	send(t, c, statusDatagram(testStatus).b)
	if e := g.next(); e.Kind != EventMessage || e.Msg != testStatus || e.From != addrOf(c) {
		t.Fatalf("got %+v", e)
	}
	if _, ok := g.r.Peer(); ok {
		t.Fatal("a Status fixed the peer")
	}
}

func TestLongDatagramArrivesInFull(t *testing.T) {
	g := startReceiver(t)
	c := g.client()
	adif := strings.Repeat("<call:5>XX1AA ", 300)
	w := newDatagram(12, "WSJT-X")
	w.utf8(adif)
	send(t, c, w.b)
	if e := g.next(); e.Msg != (LoggedADIF{ID: "WSJT-X", ADIF: adif}) {
		t.Fatalf("ADIF text not received in full")
	}
}

func TestListenRefusesOtherAddresses(t *testing.T) {
	if _, err := Listen(config.WSJTXConfig{ID: "WSJT-X", Listen: "0.0.0.0:0"}, nil); err == nil {
		t.Fatal("bound to a non-loopback address")
	}
}

func TestListenLocalhostBindsIPv4Loopback(t *testing.T) {
	r, err := Listen(config.WSJTXConfig{ID: "WSJT-X", Listen: "localhost:0"}, nil)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer r.Close()
	if r.Addr().Addr() != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("bound to %v", r.Addr())
	}
}
