// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"time"

	"github.com/kgbvax/auto7/internal/tx/send"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// Request is a typed request to the gate. The other packages never send
// a command to WSJT-X themselves. They hand a request to the gate, and
// the gate decides it. The request types are Call, Answer, GiveUp, CQ,
// OffsetMove, BandChange and Highlight.
type Request interface {
	isRequest()
}

// Call asks the gate to call a station that sent CQ, RR73, RRR or 73.
// The gate refuses it while the chase-queue state is stale.
type Call struct {
	// Decode is the decode of the station, exactly as WSJT-X sent it. The
	// gate repeats it in a Reply. Decode.Time is the start of the period
	// of the decode, and Decode.DeltaFreq is the TX offset of the station.
	Decode wsjtx.Decode
	// Heard is the time at which auto7 received the decode.
	Heard time.Time
}

// Answer asks the gate to answer a station that calls the operator. The
// gate allows it also while the chase-queue state is stale.
type Answer struct {
	// Decode is the decode of the caller, exactly as WSJT-X sent it.
	Decode wsjtx.Decode
	// Heard is the time at which auto7 received the decode.
	Heard time.Time
}

// GiveUp tells the gate that the hunt stops to call a station. If no new
// Reply follows in the same period, the gate sends HaltTx with auto only
// true, so WSJT-X ends after the current transmission.
type GiveUp struct {
	// Call is the station that the hunt gives up.
	Call string
}

// CQ asks the gate to start a CQ run. It needs the ws-hunter build.
type CQ struct {
	// Directed true sends "CQ DX" with the own call and grid. Directed
	// false sends the plain CQ of WSJT-X.
	Directed bool
	// Periods is the number of CQ transmissions in the run. After the
	// last one the gate ends the run.
	Periods int
}

// OffsetMove asks the gate to move the own TX offset. It needs the
// ws-hunter build.
type OffsetMove struct {
	// TxDF is the new TX offset in Hz, from 200 to 4000.
	TxDF uint32
}

// BandChange asks the gate to change band and mode. It needs the
// ws-hunter build.
type BandChange struct {
	// Band is the band, for example "20m".
	Band string
	// Mode is FT8 or FT4.
	Mode string
}

// Color is a highlight color for WSJT-X.
type Color = send.Color

// RGB gives a valid highlight color.
func RGB(r, g, b uint8) Color { return send.RGB(r, g, b) }

// Highlight asks the gate to highlight a call in the WSJT-X band
// activity. It does not transmit.
type Highlight struct {
	Call   string
	Bg, Fg send.Color
	// Last true highlights only the last decode of the call.
	Last bool
}

func (Call) isRequest()       {}
func (Answer) isRequest()     {}
func (GiveUp) isRequest()     {}
func (CQ) isRequest()         {}
func (OffsetMove) isRequest() {}
func (BandChange) isRequest() {}
func (Highlight) isRequest()  {}

// Decision is the answer of the gate to one request.
type Decision struct {
	// OK is true when the gate accepted the request and sent its
	// commands.
	OK bool
	// Reason says why the gate refused the request. It is empty when OK
	// is true.
	Reason string
}

// Refusal is the last refused request, for the operator page.
type Refusal struct {
	// Kind names the request type, for example "call".
	Kind   string
	Reason string
	At     time.Time
}

// Reasons of refusals. The page shows them as they are.
const (
	ReasonStopped     = "auto7 is stopped"
	ReasonSettling    = "auto7 waits for WSJT-X on the new band"
	ReasonTooLate     = "too late for this slot"
	ReasonChaseStale  = "chase-queue state stale"
	ReasonReduced     = "reduced mode: stock WSJT-X"
	ReasonNoStatus    = "no WSJT-X status yet"
	ReasonTooClose    = "TX offset too close to the offset of the station"
	ReasonCQLimit     = "CQ run longer than the limit"
	ReasonCQListen    = "listen periods after a CQ run"
	ReasonCQActive    = "a CQ run is active"
	ReasonNoOwnCall   = "WSJT-X reports no own call or grid"
	ReasonOffsetRange = "TX offset outside 200 to 4000 Hz"
	ReasonOffsetLimit = "offset moves per hour at the limit"
	ReasonSendFailed  = "send to WSJT-X failed"
	// ReasonSpecialOp refuses calls, answers and CQ while WSJT-X runs a
	// special operating mode, such as Fox or Hound.
	ReasonSpecialOp = "WSJT-X is in a special operating mode"
	// ReasonQSORuns refuses a request that would stop the running QSO.
	ReasonQSORuns       = "a QSO runs"
	ReasonTransmits     = "WSJT-X transmits"
	ReasonBandUnknown   = "WSJT-X has no default dial for this band and mode"
	ReasonBandNotListed = "band and mode are not in the band list"
	ReasonBandChanging  = "a band change is in progress"
	// ReasonNoHalt drops a band change when WSJT-X does not turn Enable
	// Tx off within two periods after HaltTx.
	ReasonNoHalt    = "WSJT-X did not turn Enable Tx off"
	ReasonOldDecode = "decode from before the band change"
)

// Reasons of stops. Each stop latches until the operator enables auto7
// again.
const (
	ReasonOperator    = "stopped by the operator"
	ReasonBusCommand  = "stop command on the bus"
	ReasonNoPresence  = "operator not present"
	ReasonHeartbeat   = "WSJT-X heartbeat stale"
	ReasonPeerChanged = "WSJT-X address changed"
	ReasonSecond      = "second controller"
	ReasonHoldOff     = "Hold Tx Freq off"
	ReasonAutoTxOff   = "automatic Tx enable off"
	ReasonExit        = "auto7 exits"
	ReasonNotStopped  = "WSJT-X did not stop"
	ReasonNoPeerAddr  = "WSJT-X address unknown"
	ReasonTakeover    = "operator took over"
	ReasonRestarted   = "WSJT-X restarted"
	// ReasonNoDialChange stops when WSJT-X does not report the new dial
	// and mode within DialTimeout after a band change.
	ReasonNoDialChange = "WSJT-X did not change band"
	// ReasonTxInBandChange stops when WSJT-X transmits before the settle
	// wait after a band change ends.
	ReasonTxInBandChange = "WSJT-X transmitted during the band change"
	// ReasonListsUnreadable refuses Enable while the hunt lists cannot be
	// read. Without the worked list, auto7 can call a station again that
	// it just worked.
	ReasonListsUnreadable = "lists unreadable"
)
