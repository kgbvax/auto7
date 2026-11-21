// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"time"

	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// Input ages. An input older than its age is stale.
const (
	// HeartbeatTTL is the age of the WSJT-X heartbeat. A stale heartbeat
	// stops auto7.
	HeartbeatTTL = 30 * time.Second
	// ChaseTTL is the age of the chase-queue state. A stale chase state
	// pauses calls. auto7 still answers callers.
	ChaseTTL = 75 * time.Second
	// PeerTTL is the age of the state of another auto7 host. Only a state
	// with a newer ts counts as a second controller.
	PeerTTL = 90 * time.Second
)

// inputs holds the latest value and receive time of each input of the
// gate. Only the owner goroutine of the gate uses it.
type inputs struct {
	ownHost  string
	presence *presence

	linkUp bool

	heartbeat time.Time
	build     wsjtx.Build
	// hbVersion and hbRevision are the version and the revision of the
	// last Heartbeat. A change shows a WSJT-X restart.
	hbVersion, hbRevision string
	hbSeen                bool
	status                wsjtx.Status
	hasStatus             bool

	chase time.Time

	// peerState and peerStatus hold the last state and status per auto7
	// host other than the own host.
	peerState  map[string]bus.PeerState
	peerStatus map[string]string
}

func newInputs(ownHost string, presenceHosts []string) *inputs {
	return &inputs{
		ownHost:    normHost(ownHost),
		presence:   newPresence(presenceHosts),
		peerState:  map[string]bus.PeerState{},
		peerStatus: map[string]string{},
	}
}

// handleBus records one bus event. A retained delivery never refreshes
// the age of an input, because it can be old.
func (in *inputs) handleBus(e bus.Event) {
	switch e := e.(type) {
	case bus.Link:
		in.linkUp = e.Up
	case bus.Presence:
		in.presence.handle(e)
	case bus.ChaseState:
		if !e.Retained {
			in.chase = e.Received
		}
	case bus.PeerState:
		if normHost(e.Host) != in.ownHost {
			in.peerState[normHost(e.Host)] = e
		}
	case bus.PeerStatus:
		if normHost(e.Host) != in.ownHost {
			in.peerStatus[normHost(e.Host)] = e.Value
		}
	}
}

// handleWSJTX records one WSJT-X event. The caller handles
// wsjtx.EventPeerChanged, because its message is not an input.
func (in *inputs) handleWSJTX(e wsjtx.Event) {
	switch m := e.Msg.(type) {
	case wsjtx.Heartbeat:
		in.heartbeat = e.At
		in.build = m.Build()
		in.hbVersion, in.hbRevision, in.hbSeen = m.Version, m.Revision, true
	case wsjtx.Status:
		in.status = m
		in.hasStatus = true
	}
}

// fresh reports whether an input received at at is at most ttl old.
func fresh(at, now time.Time, ttl time.Duration) bool {
	return !at.IsZero() && now.Sub(at) <= ttl
}

// blocker gives the first reason that stops auto7 and refuses Enable. It
// is empty when every input that TX needs is live.
func (in *inputs) blocker(now time.Time) string {
	if _, ok := in.presence.host(now); !ok {
		return ReasonNoPresence
	}
	if !in.linkUp {
		return bus.BusDown
	}
	if !fresh(in.heartbeat, now, HeartbeatTTL) {
		return ReasonHeartbeat
	}
	if host, ok := in.secondController(now); ok {
		return ReasonSecond + " " + host
	}
	return ""
}

// chaseFresh reports whether the chase-queue state is live.
func (in *inputs) chaseFresh(now time.Time) bool { return fresh(in.chase, now, ChaseTTL) }

// secondController gives another auto7 host that reports enabled, with
// status online and a payload ts at most PeerTTL old. With more than one
// such host it gives the first by name.
func (in *inputs) secondController(now time.Time) (host string, ok bool) {
	for h, s := range in.peerState {
		if !s.Enabled || in.peerStatus[h] != bus.StatusOnline || !fresh(s.TS, now, PeerTTL) {
			continue
		}
		if !ok || h < host {
			host, ok = h, true
		}
	}
	return host, ok
}
