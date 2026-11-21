// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tx is the TX control of auto7. It holds the gate. Every
// request that can make WSJT-X transmit goes through the gate, and only
// the gate sends commands to WSJT-X.
//
// The gate state has one owner goroutine, Run. Other goroutines hand it
// requests and inputs over channels. auto7 starts in Stopped, and every
// stop latches until the operator enables auto7 again.
package tx

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/tx/send"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// Conn is the UDP send handle of the gate. *net.UDPConn implements it.
// Only cmd/auto7 builds it, and it gives it only to the gate.
type Conn = send.Conn

// StatePublisher publishes the own state on the host slot. *bus.Client
// implements it.
type StatePublisher interface {
	PublishState(ctx context.Context, s bus.State) error
}

// ErrClosed is the error of a call after Run returned.
var ErrClosed = errors.New("tx: gate is not running")

// tickEvery is the interval of the timer checks without a Ticks channel.
const tickEvery = 500 * time.Millisecond

// Options holds the inputs and the handles of the gate.
type Options struct {
	// Config gives the own host, the presence hosts, the WSJT-X Id and
	// the limits.
	Config config.Config
	// Conn is the UDP send handle.
	Conn Conn
	// Peer gives the WSJT-X address. Use the Peer method of the WSJT-X
	// receiver.
	Peer func() (netip.AddrPort, bool)
	// ResetPeer lets the receiver learn a new WSJT-X address. The gate
	// calls it only while it is stopped. Use the ResetPeer method of the
	// WSJT-X receiver.
	ResetPeer func()
	// Publisher publishes the own state. Nil means no publish.
	Publisher StatePublisher
	// WSJTX delivers the events of the WSJT-X receiver.
	WSJTX <-chan wsjtx.Event
	// Bus delivers the events of the bus client. When it closes, the
	// gate treats the bus link as lost.
	Bus <-chan bus.Event
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Ticks drives the timer checks. Nil means a ticker every 500 ms.
	Ticks <-chan time.Time
	// Logger receives the log lines. Nil means slog.Default.
	Logger *slog.Logger
	// Task gives the current task of the hunt. The gate publishes it with
	// its state. It can be nil.
	Task func() string
	// ListsErr reports an error while the hunt lists cannot be read. The
	// gate then refuses Enable. It can be nil.
	ListsErr func() error
}

// call is one function that runs on the owner goroutine.
type call struct {
	fn  func(*core) any
	out chan any
}

// Gate is the TX gate.
type Gate struct {
	opts  Options
	core  *core
	calls chan call
	pub   chan bus.State
	state atomic.Pointer[State]
	done  chan struct{}
	log   *slog.Logger
}

// New builds a gate. Call Run to start it.
func New(opts Options) *Gate {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	g := &Gate{
		opts:  opts,
		calls: make(chan call),
		pub:   make(chan bus.State, 1),
		done:  make(chan struct{}),
		log:   log,
	}
	g.core = newCore(coreOptions{
		Config:    opts.Config,
		Conn:      opts.Conn,
		Peer:      opts.Peer,
		ResetPeer: opts.ResetPeer,
		Now:       opts.Now,
		Publish:   g.offer,
		Logger:    log,
		Task:      opts.Task,
		ListsErr:  opts.ListsErr,
	})
	g.store()
	return g
}

// offer hands the newest state to the publisher goroutine. It never
// blocks, and an older state that waits is replaced.
func (g *Gate) offer(s bus.State) {
	select {
	case <-g.pub:
	default:
	}
	select {
	case g.pub <- s:
	default:
	}
}

func (g *Gate) store() {
	s := g.core.snapshot()
	g.state.Store(&s)
}

// Run is the owner goroutine of the gate state. It returns when ctx
// ends. Before it returns, it stops an armed gate, so WSJT-X gets HaltTx
// before auto7 exits. Close the Conn only after Run returned.
func (g *Gate) Run(ctx context.Context) error {
	defer close(g.done)
	ticks := g.opts.Ticks
	if ticks == nil {
		t := time.NewTicker(tickEvery)
		defer t.Stop()
		ticks = t.C
	}
	if g.opts.Publisher != nil {
		go g.publish(ctx)
	}
	wsjtxCh, busCh := g.opts.WSJTX, g.opts.Bus
	for {
		select {
		case <-ctx.Done():
			g.core.shutdown()
			g.store()
			return nil
		case e, ok := <-wsjtxCh:
			if !ok {
				wsjtxCh = nil
				continue
			}
			g.core.handleWSJTX(e)
		case e, ok := <-busCh:
			if !ok {
				busCh = nil
				g.core.handleBus(bus.Link{Up: false, At: g.core.now(), Reason: bus.BusDown})
				break
			}
			g.core.handleBus(e)
		case <-ticks:
			g.core.tick()
		case c := <-g.calls:
			v := c.fn(g.core)
			// Store first, so the caller sees the new state when it
			// gets the result.
			g.store()
			c.out <- v
			continue
		}
		g.store()
	}
}

// publish sends the offered states to the publisher until ctx ends.
func (g *Gate) publish(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-g.pub:
			if err := g.opts.Publisher.PublishState(ctx, s); err != nil && ctx.Err() == nil {
				g.log.Warn("tx: state publish failed", "err", err)
			}
		}
	}
}

// do runs fn on the owner goroutine and waits for its result.
func (g *Gate) do(ctx context.Context, fn func(*core) any) (any, error) {
	c := call{fn: fn, out: make(chan any, 1)}
	select {
	case g.calls <- c:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.done:
		return nil, ErrClosed
	}
	select {
	case v := <-c.out:
		return v, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Enable arms the gate on a request of the operator. It returns the
// reason when the gate refuses, for example "operator not present".
func (g *Gate) Enable(ctx context.Context) error {
	v, err := g.do(ctx, func(c *core) any { return c.enable() })
	if err != nil {
		return err
	}
	if v != nil {
		return v.(error)
	}
	return nil
}

// Stop stops the gate on a request of the operator. The gate sends
// HaltTx at once.
func (g *Gate) Stop(ctx context.Context) error {
	_, err := g.do(ctx, func(c *core) any { c.operatorStop(ReasonOperator); return nil })
	return err
}

// Submit hands one request to the gate and gives its decision.
func (g *Gate) Submit(ctx context.Context, r Request) Decision {
	v, err := g.do(ctx, func(c *core) any { return c.submit(r) })
	if err != nil {
		return Decision{Reason: err.Error()}
	}
	return v.(Decision)
}

// State gives the newest state of the gate. It never blocks.
func (g *Gate) State() State { return *g.state.Load() }
