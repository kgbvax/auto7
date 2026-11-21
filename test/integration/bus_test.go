// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/config"
)

// busHost is the host of the bus client in these tests. It differs from
// testHost, so the tests do not touch the slot of the auto7 binary.
const busHost = "itbus"

// connectBus connects a bus client through the link relay of the env.
func connectBus(t *testing.T, e *Env) *bus.Client {
	t.Helper()
	cfg := config.Config{
		Site:    testSite,
		Station: testStation,
		Host:    busHost,
		MQTT:    config.MQTTConfig{Broker: e.Link.URL(), ClientID: "it-bus-" + t.Name()},
	}
	c := bus.New(cfg, bus.Options{Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// watch subscribes the peer connection to the topic and delivers each
// payload.
func watch(t *testing.T, e *Env, topic string) <-chan string {
	t.Helper()
	ch := make(chan string, 16)
	tok := e.Conn.Subscribe(topic, 1, func(_ pahomqtt.Client, m pahomqtt.Message) {
		select {
		case ch <- string(m.Payload()):
		default:
		}
	})
	if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		t.Fatalf("subscribe %s: %v", topic, tok.Error())
	}
	t.Cleanup(func() { e.Conn.Unsubscribe(topic).WaitTimeout(time.Second) })
	return ch
}

// waitPayload waits up to 10 s for the payload on ch.
func waitPayload(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case got := <-ch:
			if got == want {
				return
			}
		case <-timeout:
			t.Fatalf("no %q within 10 s", want)
		}
	}
}

// nextEvent waits up to 10 s for an event of type T that pred accepts.
func nextEvent[T bus.Event](t *testing.T, c *bus.Client, pred func(T) bool) T {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-c.Events():
			if e, ok := ev.(T); ok && (pred == nil || pred(e)) {
				return e
			}
		case <-timeout:
			var zero T
			t.Fatalf("no %T within 10 s", zero)
		}
	}
}

func TestBusWillOfflineAfterDrop(t *testing.T) {
	e := newEnv(t)
	topics := bus.TopicsFor(testSite, testStation, busHost)
	t.Cleanup(func() {
		e.Conn.Publish(topics.Status, 1, true, []byte{}).WaitTimeout(time.Second)
	})
	status := watch(t, e, topics.Status)

	c := connectBus(t, e)
	waitPayload(t, status, bus.StatusOnline)

	e.Link.Cut()
	waitPayload(t, status, bus.StatusOffline)
	down := nextEvent(t, c, func(l bus.Link) bool { return !l.Up })
	if down.Reason != bus.BusDown {
		t.Errorf("link event = %+v, want %q", down, bus.BusDown)
	}

	e.Link.Restore()
	nextEvent(t, c, func(l bus.Link) bool { return l.Up && l.Conn == 2 })
	waitPayload(t, status, bus.StatusOnline)
}

func TestBusReadsPeers(t *testing.T) {
	e := newEnv(t)
	t.Cleanup(func() {
		for _, s := range e.Station.Slots() {
			_ = s.Clear()
		}
	})
	if err := e.Station.SetRadio("20m", true, false); err != nil {
		t.Fatal(err)
	}
	if err := e.Station.SetAntSwitch("port4", true); err != nil {
		t.Fatal(err)
	}
	c := connectBus(t, e)

	r := nextEvent[bus.RadioState](t, c, nil)
	if !r.Retained || !r.TX || r.Band != "20m" || r.TS.IsZero() || r.Received.IsZero() {
		t.Errorf("retained radio state = %+v, want retained 20m tx with ts", r)
	}
	if err := e.Station.SetAntSwitch("port6", false); err != nil {
		t.Fatal(err)
	}
	a := nextEvent(t, c, func(s bus.AntSwitchState) bool { return s.Selected == "port6" })
	if a.Retained || a.Settled {
		t.Errorf("live ant-switch state = %+v, want live and not settled", a)
	}

	if err := e.Presence.Send(true, false); err != nil {
		t.Fatal(err)
	}
	p := nextEvent[bus.Presence](t, c, nil)
	if p.Retained || !p.Present || p.Host != testHost {
		t.Errorf("presence = %+v, want live present from %s", p, testHost)
	}
}
