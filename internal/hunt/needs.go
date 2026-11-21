// SPDX-License-Identifier: AGPL-3.0-or-later

// Package hunt joins the decodes of WSJT-X with the needs of chase-queue,
// publishes the decoded stations and the logged QSOs, and keeps the local
// worked and skip lists.
package hunt

import (
	"strings"
	"sync"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/tx"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// MsgType is the type of an FT8 or FT4 message.
type MsgType string

// Message types. The candidate topic carries them as text.
const (
	MsgCQ     MsgType = "CQ"
	MsgReport MsgType = "report"
	MsgRR73   MsgType = "RR73"
	MsgRRR    MsgType = "RRR"
	Msg73     MsgType = "73"
	MsgGrid   MsgType = "grid"
	// MsgOther is a message with a known sender and no other known type,
	// for example two calls without a grid.
	MsgOther MsgType = "other"
)

// Message is a parsed FT8 or FT4 message.
type Message struct {
	Type MsgType
	// From is the call of the sender.
	From string
	// To is the call that the sender addresses. It is empty for a CQ.
	To string
	// Grid is the locator, if the message carries one.
	Grid string
	// Report is the signal report, for example "-10" or "R+05".
	Report string
	// CQModifier is the word after CQ, for example "DX".
	CQModifier string
	// FoxForm is true for the Fox compound form
	// "<call1> RR73; <call2> <fox> <report>". Then From is the Fox, To is
	// call1 and Second is call2.
	FoxForm bool
	Second  string
}

// ParseMessage parses the free text of a decode. It returns false when
// the text has no sender call, for example free text or a hashed call
// that WSJT-X did not resolve.
func ParseMessage(text string) (Message, bool) {
	tok := strings.Fields(strings.ToUpper(text))
	var m Message
	switch {
	case len(tok) >= 4 && tok[1] == "RR73;":
		if !isCall(tok[0]) || !isCall(tok[2]) || !isCall(tok[3]) {
			return Message{}, false
		}
		m = Message{Type: MsgRR73, From: normCall(tok[3]), To: normCall(tok[0]), Second: normCall(tok[2]), FoxForm: true}
		if len(tok) >= 5 && isReport(tok[4]) {
			m.Report = tok[4]
		}
	case len(tok) >= 2 && tok[0] == "CQ":
		rest := tok[1:]
		if len(rest) >= 2 && isCQModifier(rest[0]) && isCall(rest[1]) {
			m.CQModifier = rest[0]
			rest = rest[1:]
		}
		if !isCall(rest[0]) {
			return Message{}, false
		}
		m.Type, m.From = MsgCQ, normCall(rest[0])
		if len(rest) >= 2 && isGrid(rest[1]) {
			m.Grid = rest[1]
		}
	case len(tok) >= 2:
		if !isCall(tok[0]) || !isCall(tok[1]) {
			return Message{}, false
		}
		m = Message{Type: MsgOther, From: normCall(tok[1]), To: normCall(tok[0])}
		if len(tok) >= 3 {
			x := tok[2]
			switch {
			case x == "RR73":
				m.Type = MsgRR73
			case x == "RRR":
				m.Type = MsgRRR
			case x == "73":
				m.Type = Msg73
			case isReport(x):
				m.Type, m.Report = MsgReport, x
			case isGrid(x):
				m.Type, m.Grid = MsgGrid, x
			case x == "R" && len(tok) >= 4 && isGrid(tok[3]):
				m.Type, m.Grid = MsgGrid, tok[3]
			}
		}
	default:
		return Message{}, false
	}
	return m, true
}

// isCall reports whether s looks like a call sign, with or without angle
// brackets. A call has letters and digits and is not a grid or RR73.
func isCall(s string) bool {
	if strings.HasPrefix(s, "<") != strings.HasSuffix(s, ">") {
		return false
	}
	s = strings.Trim(s, "<>")
	if len(s) < 3 || len(s) > 13 || s == "RR73" || isGrid(s) {
		return false
	}
	var letter, digit bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			letter = true
		case r >= '0' && r <= '9':
			digit = true
		case r == '/':
		default:
			return false
		}
	}
	return letter && digit
}

// isGrid reports whether s is a four character Maidenhead locator.
func isGrid(s string) bool {
	return len(s) == 4 && s[0] >= 'A' && s[0] <= 'R' && s[1] >= 'A' && s[1] <= 'R' &&
		s[2] >= '0' && s[2] <= '9' && s[3] >= '0' && s[3] <= '9'
}

// isReport reports whether s is a signal report such as "-10" or "R+05".
func isReport(s string) bool {
	s = strings.TrimPrefix(s, "R")
	return len(s) == 3 && (s[0] == '+' || s[0] == '-') && s[1] >= '0' && s[1] <= '9' && s[2] >= '0' && s[2] <= '9'
}

// isCQModifier reports whether s is a directed CQ word: one to four
// letters, or three digits.
func isCQModifier(s string) bool {
	if len(s) == 3 && strings.Trim(s, "0123456789") == "" {
		return true
	}
	return len(s) >= 1 && len(s) <= 4 && strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == ""
}

// BandOf returns the band name of a frequency in Hz, for example "20m".
// It returns "" outside the bands from 160 m to 6 m. It knows 60m, which
// the band table of the gate leaves out.
func BandOf(hz uint64) string {
	if hz >= 5_250_000 && hz <= 5_450_000 {
		return "60m"
	}
	return tx.BandOf(hz)
}

// ModeOf returns FT8 or FT4 for a decode. decodeMode is the mode symbol
// of the decode ("~" for FT8, "+" for FT4). statusMode is the mode of
// the last Status. It returns "" for any other mode.
func ModeOf(decodeMode, statusMode string) string {
	switch decodeMode {
	case "~":
		return config.ModeFT8
	case "+":
		return config.ModeFT4
	case "":
		if statusMode == config.ModeFT8 || statusMode == config.ModeFT4 {
			return statusMode
		}
	}
	return ""
}

// HighValue reports whether a chase-queue row is high value: a need is
// an all-time new one, or the row is a first-tier expedition.
func HighValue(r bus.ChaseRow) bool { return r.ATNO() || r.FirstTierExpedition() }

// Reasons for "highlight only". auto7 highlights such a station and
// shows it, but does not call it.
const (
	HighlightFoxHoundDial = "Fox/Hound dial"
	HighlightFoxForm      = "Fox compound message"
	HighlightSpecialOp    = "special operating mode"
)

// FoxHoundPassbandHz is the width above a Fox/Hound dial in which a spot
// counts as a spot on that dial.
const FoxHoundPassbandHz = 3000

// Station is one decoded station joined with the needs of chase-queue
// and the local lists.
type Station struct {
	Call string
	// To is the call that the station addressed. It is empty for a CQ.
	To   string
	Band string
	// Mode is FT8 or FT4.
	Mode    string
	LastMsg MsgType
	Grid    string
	// Message is the full text of the decode.
	Message string
	// DialHz is the dial frequency at the decode.
	DialHz    uint64
	DeltaFreq uint32
	SNR       int32
	// Period is the start of the period since midnight UTC.
	Period time.Duration
	// HeardAt is the local receive time of the decode.
	HeardAt time.Time
	// Row is the chase-queue row with the same call and band, or nil.
	Row *bus.ChaseRow
	// Needed is true when chase-queue marks the station as needed on
	// this band and auto7 did not work it on this band.
	Needed bool
	// HighValue is true for a needed station with an all-time new need
	// or a first-tier expedition.
	HighValue bool
	// Worked is true when the worked list holds the call on this band.
	Worked bool
	// Skipped is true while the call is on the skip list.
	Skipped bool
	// HighlightOnly is true when auto7 must not call the station.
	// Highlight gives the reason.
	HighlightOnly bool
	Highlight     string
}

// key identifies a station on a band.
type key struct{ call, band string }

// Needs joins decodes with chase-queue rows and the local lists. All
// methods are safe for use by many goroutines.
type Needs struct {
	lists    *Lists
	foxDials []uint64

	mu     sync.Mutex
	status wsjtx.Status
	band   string
	rows   map[key][]bus.ChaseRow
	// foxes holds the stations that sent the Fox compound form during
	// the current band visit.
	foxes map[key]bool
}

// NewNeeds returns a Needs that uses lists and the Fox/Hound dials of
// the configuration.
func NewNeeds(lists *Lists, foxDialsHz []uint64) *Needs {
	return &Needs{
		lists:    lists,
		foxDials: append([]uint64(nil), foxDialsHz...),
		rows:     map[key][]bus.ChaseRow{},
		foxes:    map[key]bool{},
	}
}

// Lists returns the local lists.
func (n *Needs) Lists() *Lists { return n.lists }

// ListsErr returns an error that wraps ErrListsUnreadable when the list
// file cannot be read, else nil.
func (n *Needs) ListsErr() error { return n.lists.Err() }

// SetStatus records the last WSJT-X Status. A change of band starts a new
// band visit and forgets the Fox marks of the old visit.
func (n *Needs) SetStatus(st wsjtx.Status) {
	n.mu.Lock()
	defer n.mu.Unlock()
	band := BandOf(st.DialHz)
	if band != n.band {
		n.foxes = map[key]bool{}
	}
	n.status, n.band = st, band
}

// SetChase records the last chase-queue state.
func (n *Needs) SetChase(cs bus.ChaseState) {
	rows := map[key][]bus.ChaseRow{}
	for _, r := range cs.Ranked {
		if r.Call == "" {
			continue
		}
		k := key{normCall(r.Call), r.Band}
		rows[k] = append(rows[k], r)
	}
	n.mu.Lock()
	n.rows = rows
	n.mu.Unlock()
}

// onFoxHoundDial reports whether hz lies on a Fox/Hound dial or in the
// passband above it.
func (n *Needs) onFoxHoundDial(hz uint64) bool {
	for _, d := range n.foxDials {
		if hz >= d && hz < d+FoxHoundPassbandHz {
			return true
		}
	}
	return false
}

// SpotHighlightOnly reports whether a chase-queue spot lies on a
// Fox/Hound dial. auto7 does not follow such a spot.
func (n *Needs) SpotHighlightOnly(r bus.ChaseRow) bool {
	return r.FreqHz > 0 && n.onFoxHoundDial(uint64(r.FreqHz))
}

// Observe joins one decode with the last Status, the chase-queue rows
// and the local lists. at is the local receive time. It returns false
// when the decode has no sender, no band from 160 m to 6 m, or a mode
// other than FT8 or FT4.
func (n *Needs) Observe(d wsjtx.Decode, at time.Time) (Station, bool) {
	m, ok := ParseMessage(d.Message)
	if !ok {
		return Station{}, false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	st := n.status
	mode := ModeOf(d.Mode, st.Mode)
	if n.band == "" || mode == "" {
		return Station{}, false
	}
	s := Station{
		Call: m.From, To: m.To, Band: n.band, Mode: mode, LastMsg: m.Type, Grid: m.Grid,
		Message: d.Message, DialHz: st.DialHz, DeltaFreq: d.DeltaFreq, SNR: d.SNR,
		Period: d.Time, HeardAt: at,
	}
	k := key{s.Call, s.Band}
	for _, r := range n.rows[k] {
		if s.Row == nil || (!s.Row.Needed && r.Needed) {
			s.Row = &r
		}
		if r.Needed || HighValue(r) {
			s.Needed = true
		}
		if HighValue(r) {
			s.HighValue = true
		}
	}
	s.Worked = n.lists.Worked(s.Call, s.Band)
	if s.Worked {
		s.Needed, s.HighValue = false, false
	}
	s.Skipped = n.lists.Skipped(s.Call)
	if m.FoxForm {
		n.foxes[k] = true
	}
	switch {
	case st.SpecialOp != wsjtx.SpecialOpNone:
		s.HighlightOnly, s.Highlight = true, HighlightSpecialOp
	case n.onFoxHoundDial(st.DialHz):
		s.HighlightOnly, s.Highlight = true, HighlightFoxHoundDial
	case n.foxes[k]:
		s.HighlightOnly, s.Highlight = true, HighlightFoxForm
	}
	return s, true
}

// Logged records a logged QSO on the worked list and returns it in the
// form of the QSO topic. It returns the QSO also when the write fails.
func (n *Needs) Logged(q wsjtx.QSOLogged) (bus.QSO, error) {
	out := QSOOf(q)
	at := q.TimeOn
	if at.IsZero() {
		at = q.TimeOff
	}
	if at.IsZero() {
		at = n.lists.now()
	}
	return out, n.lists.AddWorked(out.Call, out.Band, out.Mode, at)
}

// QSOOf converts a WSJT-X QSOLogged into the form of the QSO topic.
func QSOOf(q wsjtx.QSOLogged) bus.QSO {
	return bus.QSO{
		Call: normCall(q.DXCall), Grid: q.DXGrid, Band: BandOf(q.TxFreqHz), FreqHz: q.TxFreqHz,
		Mode: q.Mode, TimeOn: q.TimeOn, TimeOff: q.TimeOff,
		RSTSent: q.ReportSent, RSTRcvd: q.ReportReceived, MyCall: q.MyCall, MyGrid: q.MyGrid,
	}
}
