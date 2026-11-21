// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
)

func presenceMsg(host string, present, retained bool, at time.Time) bus.Presence {
	return bus.Presence{Meta: bus.Meta{Topic: "site/hf/logotter/presence", Retained: retained, Received: at, TS: at}, Present: present, Host: host}
}

func TestPresenceCountsListedHostsFor90s(t *testing.T) {
	p := newPresence([]string{"op-pc", "op-laptop"})
	p.handle(presenceMsg("OP-PC ", true, false, t0))
	if h, ok := p.host(t0.Add(90 * time.Second)); !ok || h != "op-pc" {
		t.Fatalf("host %q %v at 90 s, want op-pc", h, ok)
	}
	if h, ok := p.host(t0.Add(91 * time.Second)); ok {
		t.Fatalf("host %q still present at 91 s", h)
	}
}

func TestPresenceIgnoresRetainedAndUnlisted(t *testing.T) {
	p := newPresence([]string{"op-pc"})
	p.handle(presenceMsg("op-pc", true, true, t0))
	p.handle(presenceMsg("other-pc", true, false, t0))
	if h, ok := p.host(t0); ok {
		t.Fatalf("present by %q", h)
	}
}

func TestPresenceFalseEndsTheHost(t *testing.T) {
	p := newPresence([]string{"op-pc", "op-laptop"})
	p.handle(presenceMsg("op-pc", true, false, t0))
	p.handle(presenceMsg("op-laptop", true, false, t0.Add(10*time.Second)))
	if h, _ := p.host(t0.Add(20 * time.Second)); h != "op-laptop" {
		t.Fatalf("host %q, want the newest op-laptop", h)
	}
	p.handle(presenceMsg("op-laptop", false, false, t0.Add(20*time.Second)))
	if h, _ := p.host(t0.Add(20 * time.Second)); h != "op-pc" {
		t.Fatalf("host %q, want op-pc", h)
	}
	p.handle(presenceMsg("op-pc", false, false, t0.Add(21*time.Second)))
	if h, ok := p.host(t0.Add(21 * time.Second)); ok {
		t.Fatalf("present by %q after present false", h)
	}
}
