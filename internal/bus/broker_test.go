// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/eclipse/paho.mqtt.golang/packets"
)

// fakeBroker is a small MQTT 3.1.1 broker for the unit tests. It keeps
// retained messages, routes publishes with the + and # wildcards, sends
// the will when a client connection drops without DISCONNECT, and can
// refuse logins or cut every client link.
type fakeBroker struct {
	t  *testing.T
	ln net.Listener

	mu       sync.Mutex
	retained map[string][]byte
	clients  map[*fakeConn]struct{}
	refuse   byte
	logins   []packets.ConnectPacket
	// published records every publish that a client sent.
	published []recordedPublish
}

// recordedPublish is one publish that a client sent to the broker.
type recordedPublish struct {
	Topic    string
	Payload  string
	Retained bool
}

type fakeConn struct {
	conn net.Conn
	mu   sync.Mutex
	subs []string
	will *packets.PublishPacket
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBroker{t: t, ln: ln, retained: map[string][]byte{}, clients: map[*fakeConn]struct{}{}}
	go b.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		b.Cut()
	})
	return b
}

// URL is the broker URL.
func (b *fakeBroker) URL() string { return "tcp://" + b.ln.Addr().String() }

// RefuseLogins makes the broker answer every CONNECT with the return code.
func (b *fakeBroker) RefuseLogins(code byte) {
	b.mu.Lock()
	b.refuse = code
	b.mu.Unlock()
}

// SetRetained stores a retained message as if a client had published it.
func (b *fakeBroker) SetRetained(topic, payload string) {
	b.mu.Lock()
	b.retained[topic] = []byte(payload)
	b.mu.Unlock()
}

// Retained returns the retained payload of the topic.
func (b *fakeBroker) Retained(topic string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.retained[topic]
	return string(p), ok
}

// Published returns every publish that a client sent.
func (b *fakeBroker) Published() []recordedPublish {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]recordedPublish(nil), b.published...)
}

// Logins returns every CONNECT packet.
func (b *fakeBroker) Logins() []packets.ConnectPacket {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]packets.ConnectPacket(nil), b.logins...)
}

// Send delivers a live message to every matching subscriber.
func (b *fakeBroker) Send(topic, payload string) {
	b.route(topic, []byte(payload), false)
}

// Subscribers returns the number of clients with a subscription that
// matches the topic.
func (b *fakeBroker) Subscribers(topic string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for c := range b.clients {
		c.mu.Lock()
		for _, f := range c.subs {
			if topicMatch(f, topic) {
				n++
				break
			}
		}
		c.mu.Unlock()
	}
	return n
}

// Cut closes every client link without DISCONNECT, so the broker sends
// the wills.
func (b *fakeBroker) Cut() {
	b.mu.Lock()
	conns := make([]*fakeConn, 0, len(b.clients))
	for c := range b.clients {
		conns = append(conns, c)
	}
	b.mu.Unlock()
	for _, c := range conns {
		_ = c.conn.Close()
	}
}

func (b *fakeBroker) serve() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.handle(&fakeConn{conn: conn})
	}
}

func (b *fakeBroker) handle(c *fakeConn) {
	defer c.conn.Close()
	first, err := packets.ReadPacket(c.conn)
	if err != nil {
		return
	}
	cp, ok := first.(*packets.ConnectPacket)
	if !ok {
		return
	}
	b.mu.Lock()
	b.logins = append(b.logins, *cp)
	refuse := b.refuse
	b.mu.Unlock()
	ack := packets.NewControlPacket(packets.Connack).(*packets.ConnackPacket)
	ack.ReturnCode = refuse
	if err := c.write(ack); err != nil || refuse != packets.Accepted {
		return
	}
	if cp.WillFlag {
		w := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
		w.TopicName = cp.WillTopic
		w.Payload = cp.WillMessage
		w.Retain = cp.WillRetain
		c.will = w
	}
	b.mu.Lock()
	b.clients[c] = struct{}{}
	b.mu.Unlock()
	clean := false
	defer func() {
		b.mu.Lock()
		delete(b.clients, c)
		b.mu.Unlock()
		if !clean && c.will != nil {
			b.publish(c.will.TopicName, c.will.Payload, c.will.Retain)
		}
	}()
	for {
		p, err := packets.ReadPacket(c.conn)
		if err != nil {
			return
		}
		switch p := p.(type) {
		case *packets.PublishPacket:
			if p.Qos == 1 {
				pa := packets.NewControlPacket(packets.Puback).(*packets.PubackPacket)
				pa.MessageID = p.MessageID
				if c.write(pa) != nil {
					return
				}
			}
			b.publish(p.TopicName, p.Payload, p.Retain)
		case *packets.SubscribePacket:
			sa := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
			sa.MessageID = p.MessageID
			sa.ReturnCodes = make([]byte, len(p.Topics))
			if c.write(sa) != nil {
				return
			}
			c.mu.Lock()
			c.subs = append(c.subs, p.Topics...)
			c.mu.Unlock()
			b.replay(c, p.Topics)
		case *packets.PingreqPacket:
			if c.write(packets.NewControlPacket(packets.Pingresp)) != nil {
				return
			}
		case *packets.DisconnectPacket:
			clean = true
			return
		}
	}
}

func (c *fakeConn) write(p packets.ControlPacket) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return p.Write(c.conn)
}

// publish stores a retained message and routes it live.
func (b *fakeBroker) publish(topic string, payload []byte, retain bool) {
	b.mu.Lock()
	b.published = append(b.published, recordedPublish{Topic: topic, Payload: string(payload), Retained: retain})
	if retain {
		if len(payload) == 0 {
			delete(b.retained, topic)
		} else {
			b.retained[topic] = append([]byte(nil), payload...)
		}
	}
	b.mu.Unlock()
	b.route(topic, payload, false)
}

// route sends a message to every client with a matching subscription.
func (b *fakeBroker) route(topic string, payload []byte, retain bool) {
	b.mu.Lock()
	conns := make([]*fakeConn, 0, len(b.clients))
	for c := range b.clients {
		conns = append(conns, c)
	}
	b.mu.Unlock()
	for _, c := range conns {
		c.mu.Lock()
		match := false
		for _, f := range c.subs {
			if topicMatch(f, topic) {
				match = true
				break
			}
		}
		c.mu.Unlock()
		if match {
			_ = c.write(publishPacket(topic, payload, retain))
		}
	}
}

// replay sends the retained messages that match the filters, with the
// retain flag set.
func (b *fakeBroker) replay(c *fakeConn, filters []string) {
	b.mu.Lock()
	var out []*packets.PublishPacket
	for topic, payload := range b.retained {
		for _, f := range filters {
			if topicMatch(f, topic) {
				out = append(out, publishPacket(topic, payload, true))
				break
			}
		}
	}
	b.mu.Unlock()
	for _, p := range out {
		_ = c.write(p)
	}
}

func publishPacket(topic string, payload []byte, retain bool) *packets.PublishPacket {
	p := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	p.TopicName = topic
	p.Payload = append([]byte(nil), payload...)
	p.Retain = retain
	return p
}

// topicMatch reports whether the MQTT filter matches the topic.
func topicMatch(filter, topic string) bool {
	f := strings.Split(filter, "/")
	t := strings.Split(topic, "/")
	for i, part := range f {
		if part == "#" {
			return true
		}
		if i >= len(t) {
			return false
		}
		if part != "+" && part != t[i] {
			return false
		}
	}
	return len(f) == len(t)
}
