// SPDX-License-Identifier: AGPL-3.0-or-later

package hunt

import "github.com/kgbvax/auto7/internal/config"

// The rules in this file decide when auto7 may call a station on the air.
// They look only at the decodes of the current period.

// endsQSO reports whether a message of type t shows that the sender is
// free for a call: a CQ, or the end of a QSO with RR73, RRR or 73.
func endsQSO(t MsgType) bool {
	switch t {
	case MsgCQ, MsgRR73, MsgRRR, Msg73:
		return true
	}
	return false
}

// callable reports whether auto7 may call s now. The station must have
// sent CQ, RR73, RRR or 73, to any station. A station that addresses the
// own call is a caller, not a target.
func callable(s Station, own string) bool {
	return endsQSO(s.LastMsg) && (s.To == "" || s.To != own)
}

// inOtherQSO reports whether s works another station now: it addresses a
// call other than the own call, with a message that does not end the
// QSO. auto7 never calls into such a QSO.
func inOtherQSO(s Station, own string) bool {
	return s.To != "" && s.To != own && !endsQSO(s.LastMsg)
}

// isCaller reports whether s calls the operator: it addresses the own
// call with a grid, a report or two calls. RR73, RRR and 73 to the own
// call end a QSO and need no answer.
func isCaller(s Station, own string) bool {
	if own == "" || s.To != own {
		return false
	}
	switch s.LastMsg {
	case MsgGrid, MsgReport, MsgOther:
		return true
	}
	return false
}

// triesLimit returns the number of tries for one station. A high-value
// station gets more tries.
func triesLimit(l config.Limits, highValue bool) int {
	if highValue {
		return l.TriesHighValue
	}
	return l.Tries
}
