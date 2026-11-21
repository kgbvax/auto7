// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"strings"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
)

// PresenceTTL is how long a presence heartbeat of a listed host counts,
// by the receive time of auto7. logotter sends one every 30 s.
const PresenceTTL = 90 * time.Second

// presence tracks the presence heartbeats of the listed hosts. Only the
// owner goroutine of the gate uses it.
type presence struct {
	listed map[string]bool
	// seen holds the receive time of the last present:true per host.
	seen map[string]time.Time
}

func newPresence(hosts []string) *presence {
	p := &presence{listed: map[string]bool{}, seen: map[string]time.Time{}}
	for _, h := range hosts {
		p.listed[normHost(h)] = true
	}
	return p
}

func normHost(h string) string { return strings.ToLower(strings.TrimSpace(h)) }

// handle takes one heartbeat. It ignores a retained heartbeat, because it
// can be the last word of a logotter that stopped long ago. It also
// ignores a host that is not listed.
func (p *presence) handle(m bus.Presence) {
	if m.Retained {
		return
	}
	h := normHost(m.Host)
	if !p.listed[h] {
		return
	}
	if m.Present {
		p.seen[h] = m.Received
		return
	}
	delete(p.seen, h)
}

// host gives the listed host with the newest live heartbeat. ok is false
// when no host gives presence at now.
func (p *presence) host(now time.Time) (host string, ok bool) {
	var newest time.Time
	for h, at := range p.seen {
		if now.Sub(at) > PresenceTTL {
			continue
		}
		if !ok || at.After(newest) || (at.Equal(newest) && h < host) {
			host, newest, ok = h, at, true
		}
	}
	return host, ok
}
