// SPDX-License-Identifier: AGPL-3.0-or-later

package wsjtx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/kgbvax/auto7/internal/config"
)

// EventKind says what an Event reports.
type EventKind int

const (
	// EventMessage is a message from the configured WSJT-X instance. Before
	// the first Heartbeat it can come from any source. After the first
	// Heartbeat it comes from the fixed peer.
	EventMessage EventKind = iota
	// EventPeerFixed is the Heartbeat that fixed the peer address.
	EventPeerFixed
	// EventPeerChanged is a message with the configured Id from a source
	// other than the fixed peer. The receiver does not use the message as
	// an input. The caller treats it as a stale WSJT-X input.
	EventPeerChanged
)

func (k EventKind) String() string {
	switch k {
	case EventPeerFixed:
		return "peer fixed"
	case EventPeerChanged:
		return "peer changed"
	default:
		return "message"
	}
}

// Event is one result of the receiver.
type Event struct {
	Kind EventKind
	// Msg is the decoded message. It is set for every kind.
	Msg Message
	// From is the source address of the datagram.
	From netip.AddrPort
	// At is the receive time from the clock of the receiver.
	At time.Time
}

// Receiver reads WSJT-X datagrams from a UDP socket on the IPv4 loopback
// address. It accepts only the configured Id. The first Heartbeat fixes the
// peer address. The peer stays fixed until ResetPeer.
type Receiver struct {
	id   string
	conn *net.UDPConn
	now  func() time.Time

	mu    sync.Mutex
	peer  netip.AddrPort
	fixed bool
}

// Listen binds the socket to cfg.Listen. The host must be 127.0.0.1 or
// localhost, and localhost binds to 127.0.0.1. now is the clock of the
// receive times. If now is nil, Listen uses time.Now.
func Listen(cfg config.WSJTXConfig, now func() time.Time) (*Receiver, error) {
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("wsjtx listen %q: %w", cfg.Listen, err)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ap, err := netip.ParseAddrPort(net.JoinHostPort(host, port))
	if err != nil || ap.Addr().Unmap() != netip.AddrFrom4([4]byte{127, 0, 0, 1}) {
		return nil, fmt.Errorf("wsjtx listen %q: want 127.0.0.1 or localhost", cfg.Listen)
	}
	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(ap))
	if err != nil {
		return nil, fmt.Errorf("wsjtx listen: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	return &Receiver{id: cfg.ID, conn: conn, now: now}, nil
}

// Addr is the bound local address.
func (r *Receiver) Addr() netip.AddrPort {
	return r.conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

// Peer gives the address that the first Heartbeat fixed. The sender sends
// its commands to this address. ok is false before the first Heartbeat and
// after ResetPeer.
func (r *Receiver) Peer() (addr netip.AddrPort, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peer, r.fixed
}

// ResetPeer forgets the peer address, so the next Heartbeat fixes it again.
// The caller calls it only while auto7 is stopped.
func (r *Receiver) ResetPeer() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peer, r.fixed = netip.AddrPort{}, false
}

// Close closes the socket. Run then returns.
func (r *Receiver) Close() error { return r.conn.Close() }

// Run reads datagrams and sends an Event to out for each accepted
// datagram. It drops datagrams that it cannot decode, that have another Id,
// or that have a type it does not read. Run returns nil when ctx ends or
// Close closes the socket, and it closes the socket when it returns.
func (r *Receiver) Run(ctx context.Context, out chan<- Event) error {
	stop := context.AfterFunc(ctx, func() { r.conn.Close() })
	defer stop()
	defer r.conn.Close()
	buf := make([]byte, MaxDatagram)
	for {
		n, from, err := r.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("wsjtx read: %w", err)
		}
		e, ok := r.accept(buf[:n], from)
		if !ok {
			continue
		}
		select {
		case out <- e:
		case <-ctx.Done():
			return nil
		}
	}
}

// accept decodes one datagram and applies the Id and peer rules.
func (r *Receiver) accept(b []byte, from netip.AddrPort) (Event, bool) {
	m, err := Parse(b)
	if err != nil {
		if !errors.Is(err, ErrUnknownType) {
			slog.Debug("wsjtx: dropped datagram", "err", err)
		}
		return Event{}, false
	}
	if m.InstanceID() != r.id {
		return Event{}, false
	}
	from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
	e := Event{Kind: EventMessage, Msg: m, From: from, At: r.now()}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.fixed && from != r.peer:
		e.Kind = EventPeerChanged
	case !r.fixed:
		if _, ok := m.(Heartbeat); ok {
			r.peer, r.fixed = from, true
			e.Kind = EventPeerFixed
		}
	}
	return e, true
}
