// SPDX-License-Identifier: AGPL-3.0-or-later

// Package peers holds the test peers of the auto7 integration tests: a fake
// WSJT-X, fake StationA sources, a fake chase-queue, a presence sender, a
// fake horstreporter and a broker link that a test can cut. Every peer uses
// only invented calls and loopback addresses.
package peers

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	stmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
)

// BrokerEnv is the environment variable that holds the broker URL of the
// test broker, for example tcp://127.0.0.1:41883.
const BrokerEnv = "AUTO7_IT_BROKER"

// Conn is an MQTT client of a test. It does not reconnect, so a test sees
// each lost link.
type Conn struct {
	pahomqtt.Client
	lost chan error
}

// Connect opens an MQTT connection to broker with the client id.
func Connect(ctx context.Context, broker, clientID string) (*Conn, error) {
	c := &Conn{lost: make(chan error, 1)}
	opts := pahomqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetAutoReconnect(false).
		SetConnectRetry(false).
		SetConnectTimeout(5 * time.Second).
		SetConnectionLostHandler(func(_ pahomqtt.Client, err error) {
			select {
			case c.lost <- err:
			default:
			}
		})
	c.Client = pahomqtt.NewClient(opts)
	if err := stmqtt.Connect(ctx, c.Client); err != nil {
		return nil, fmt.Errorf("connect %s: %w", clientID, err)
	}
	return c, nil
}

// Lost delivers the error when the broker link drops.
func (c *Conn) Lost() <-chan error { return c.lost }

// Close ends the connection.
func (c *Conn) Close() { c.Disconnect(100) }

// publish sends one message with QoS 1 and waits for the broker.
func publish(c pahomqtt.Client, topic string, retained bool, payload []byte) error {
	tok := c.Publish(topic, 1, retained, payload)
	if !tok.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("publish %s: timeout", topic)
	}
	return tok.Error()
}

// LinkProxy is a TCP relay between MQTT clients and the broker. Cut closes
// every relayed connection and refuses new ones until Restore. A test
// points auto7 at URL to cut its broker link.
type LinkProxy struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	cut   bool
	conns map[net.Conn]struct{}
}

// NewLinkProxy listens on a free loopback port and relays to target, a
// host:port address.
func NewLinkProxy(target string) (*LinkProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &LinkProxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	go p.serve()
	return p, nil
}

// URL is the broker URL through the relay.
func (p *LinkProxy) URL() string { return "tcp://" + p.ln.Addr().String() }

// Cut closes every relayed connection and refuses new ones.
func (p *LinkProxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = true
	for c := range p.conns {
		_ = c.Close()
	}
	clear(p.conns)
}

// Restore accepts new connections again.
func (p *LinkProxy) Restore() {
	p.mu.Lock()
	p.cut = false
	p.mu.Unlock()
}

// Close stops the relay and closes every connection.
func (p *LinkProxy) Close() error {
	p.Cut()
	return p.ln.Close()
}

func (p *LinkProxy) serve() {
	for {
		in, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.relay(in)
	}
}

func (p *LinkProxy) relay(in net.Conn) {
	p.mu.Lock()
	refused := p.cut
	p.mu.Unlock()
	if refused {
		_ = in.Close()
		return
	}
	out, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = in.Close()
		return
	}
	p.mu.Lock()
	if p.cut {
		p.mu.Unlock()
		_ = in.Close()
		_ = out.Close()
		return
	}
	p.conns[in] = struct{}{}
	p.conns[out] = struct{}{}
	p.mu.Unlock()

	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go pipe(out, in)
	go pipe(in, out)
	<-done
	_ = in.Close()
	_ = out.Close()
	p.mu.Lock()
	delete(p.conns, in)
	delete(p.conns, out)
	p.mu.Unlock()
}
