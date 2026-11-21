// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"time"

	"github.com/kgbvax/auto7/internal/wsjtx"
)

// ArmState is the arm state of the gate. auto7 starts in Stopped. Every
// stop goes to Stopped, and only the operator leaves it.
type ArmState int

const (
	// Stopped means that auto7 makes no call. The gate sends nothing to
	// WSJT-X in this state, except HaltTx right after a stop.
	Stopped ArmState = iota
	// Settling means that the operator enabled auto7 and the gate waits
	// after a band change until WSJT-X reports the new dial and mode and
	// the settle time passed. The gate refuses every request but a
	// highlight in this state.
	Settling
	// Running means that the gate accepts requests.
	Running
)

func (a ArmState) String() string {
	switch a {
	case Settling:
		return "settling"
	case Running:
		return "running"
	default:
		return "stopped"
	}
}

// Armed reports whether the operator enabled auto7 and no stop came
// since.
func (a ArmState) Armed() bool { return a != Stopped }

// State is the state of the gate that the other packages read.
type State struct {
	Arm ArmState
	// StopReason is the reason for the last stop. It is empty before the
	// first stop and after Enable.
	StopReason string
	// StoppedAt is the time of the last stop.
	StoppedAt time.Time
	// NotStopped is true when WSJT-X did not confirm the last stop within
	// one period. The gate then sent HaltTx a second time.
	NotStopped bool
	// Build is the WSJT-X build from the last Heartbeat. Before the first
	// Heartbeat it is wsjtx.BuildReduced.
	Build wsjtx.Build
	// PresenceHost is the listed host that gives presence now. It is
	// empty when the operator is not present.
	PresenceHost string
	// ChaseStale is true when the chase-queue state is stale. The gate
	// then refuses calls and still allows answers.
	ChaseStale bool
	// Band is the band of the dial in the last WSJT-X Status, for
	// example "20m". It is empty before the first Status and for a dial
	// outside the band table.
	Band string
	// Mode is the mode of the last WSJT-X Status.
	Mode string
	// LastRefusal is the last refused request or Enable.
	LastRefusal Refusal
	// Hint names a likely cause of the last stop for the operator. It is
	// empty when the gate knows no likely cause.
	Hint string
	// Warning is a problem of the WSJT-X setup that does not stop auto7,
	// for example WarnAutoLogOff.
	Warning string

	// QSO is the view of the running QSO. The gate builds it from WSJT-X
	// data only, and the hunt reads it from this type.
	QSO QSOView
}

// QSOView is the view of the gate on the running QSO.
type QSOView struct {
	// Open is true while a QSO runs.
	Open bool
	// DXCall is the call of the partner, in upper case.
	DXCall string
	// Started is the receive time of the first decode from DXCall that
	// carried the own call.
	Started time.Time
	// Band is the band name, for example "20m". It is empty when the
	// band is not known.
	Band string
}

// Reduced reports whether auto7 runs in reduced mode: with a stock
// WSJT-X build, it calls no CQ, does not move its TX offset and does
// not change band.
func (s State) Reduced() bool { return s.Build != wsjtx.BuildFull }
