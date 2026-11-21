// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"time"

	"codeberg.org/kgbvax/stationa/shared/schema"
)

// BusDown is the reason text of a lost broker link.
const BusDown = "bus down"

// Slot names on the bus. They follow the StationA station integration
// model.
const (
	// SlotChase is the slot of chase-queue.
	SlotChase = "chase"
	// SlotAuto7 is the slot of auto7 itself.
	SlotAuto7 = "auto7"
)

// Status values on a /status topic.
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
)

// ActionStop is the only action that auto7 accepts on its command topic.
const ActionStop = "stop"

// Topics holds the MQTT topics of one auto7 instance.
type Topics struct {
	// State is <site>/<station>/auto7/<host>/state, retained.
	State string
	// Status is <site>/<station>/auto7/<host>/status, retained, with the
	// LWT "offline".
	Status string
	// Candidates is <site>/<station>/auto7/candidates, retained.
	Candidates string
	// QSO is <site>/<station>/auto7/qso, not retained.
	QSO string
	// Cmd is <site>/<station>/auto7/cmd. auto7 accepts only "stop" on it.
	Cmd string
	// PeerState is <site>/<station>/auto7/+/state.
	PeerState string
	// PeerStatus is <site>/<station>/auto7/+/status.
	PeerStatus string
	// Presence is <site>/<station>/logotter/presence.
	Presence string
	// ChaseState is <site>/<station>/chase/state.
	ChaseState string
	// ChaseStatus is <site>/<station>/chase/status.
	ChaseStatus string
}

// TopicsFor builds the topics for the site, station and host.
func TopicsFor(site, station, host string) Topics {
	base := schema.SlotBase(site, station, SlotAuto7)
	return Topics{
		State:       base + "/" + host + "/state",
		Status:      base + "/" + host + "/status",
		Candidates:  base + "/candidates",
		QSO:         base + "/qso",
		Cmd:         base + "/cmd",
		PeerState:   base + "/+/state",
		PeerStatus:  base + "/+/status",
		Presence:    site + "/" + station + "/logotter/presence",
		ChaseState:  schema.StateTopic(site, station, SlotChase),
		ChaseStatus: schema.StatusTopic(site, station, SlotChase),
	}
}

// Event is one item on the event channel of the client. Its type is one
// of Link, Presence, ChaseState, Status, PeerState, PeerStatus and
// StopCommand.
type Event interface {
	isEvent()
}

// Meta describes one delivered message.
type Meta struct {
	// Topic is the topic of the message.
	Topic string
	// Retained is true for a retained message that the broker replayed at
	// subscribe. A live message is never retained.
	Retained bool
	// TS is the ts field of the payload. It is zero when the payload has
	// no ts or the ts does not parse.
	TS time.Time
	// Received is the local receive time.
	Received time.Time
	// Conn is the number of the broker connection that delivered the
	// message. The first connection is 1. Each reconnect adds 1.
	Conn uint64
}

// Link reports a change of the broker link.
type Link struct {
	// Up is true when the link came up and false when it was lost.
	Up bool
	// Conn is the number of the connection that came up or was lost.
	Conn uint64
	// At is the local time of the change.
	At time.Time
	// Reason is BusDown when the link was lost, else empty.
	Reason string
	// Err is the error text of the lost link. It never holds the password.
	Err string
}

// Presence is one logotter presence heartbeat.
type Presence struct {
	Meta
	Present bool
	// Host is the host field of the payload.
	Host string
}

// Status is the /status of chase-queue.
type Status struct {
	Meta
	// Slot is the slot name, SlotChase.
	Slot string
	// Value is the payload, for example "online". An empty value means
	// that the retained status was cleared.
	Value string
}

// Online reports whether the status is "online".
func (s Status) Online() bool { return s.Value == StatusOnline }

// PeerState is the /state of an auto7 host slot. It includes the own
// host. The receiver decides which hosts to ignore.
type PeerState struct {
	Meta
	// Host is the host part of the topic.
	Host    string
	Enabled bool
}

// PeerStatus is the /status of an auto7 host slot. It includes the own
// host.
type PeerStatus struct {
	Meta
	// Host is the host part of the topic.
	Host string
	// Value is the payload, for example "online".
	Value string
}

// Online reports whether the status is "online".
func (s PeerStatus) Online() bool { return s.Value == StatusOnline }

// StopCommand is an accepted live "stop" on the command topic.
type StopCommand struct {
	Meta
	// Value is the value field of the command.
	Value string
}

func (Link) isEvent()        {}
func (Presence) isEvent()    {}
func (Status) isEvent()      {}
func (PeerState) isEvent()   {}
func (PeerStatus) isEvent()  {}
func (StopCommand) isEvent() {}
func (ChaseState) isEvent()  {}

// ChaseState is the /state of chase-queue.
//
// The row types below are a consumer copy of the chase-queue types in
// opsotter operating/chase-queue internal/bus/bus.go. They hold only the
// fields that auto7 reads. Keep them in step with
// docs/chase-queue-mqtt-api.md of chase-queue.
type ChaseState struct {
	Meta
	Seq int64
	// Activity is "dx" or "contest".
	Activity string
	// Ranked lists the rows in work order.
	Ranked   []ChaseRow
	Problems []string
}

// ChaseRow is one row of the chase-queue ranking.
type ChaseRow struct {
	RowID  int64  `json:"row_id"`
	Call   string `json:"call,omitempty"`
	Band   string `json:"band"`
	Needed bool   `json:"needed"`
	// Tag is "need", "mult" or empty.
	Tag string `json:"tag,omitempty"`
	// Needs lists the award slots that the station fills.
	Needs  []ChaseNeed `json:"needs,omitempty"`
	FreqHz int64       `json:"freq_hz,omitempty"`
	// Mode is "cw", "usb", "lsb", "am", "fm" or "data".
	Mode string `json:"mode,omitempty"`
	// Source is "bmrec" or "cluster".
	Source     string           `json:"source"`
	HeardAt    time.Time        `json:"heard_at,omitzero"`
	Buddy      bool             `json:"buddy,omitempty"`
	Expedition *ChaseExpedition `json:"expedition,omitempty"`
	Worked     *ChaseWorked     `json:"worked_before,omitempty"`
}

// ChaseNeed is one award slot that a row fills.
type ChaseNeed struct {
	Award string `json:"award"`
	Key   string `json:"key"`
	Band  string `json:"band,omitempty"`
	Mode  string `json:"mode,omitempty"`
	// Status is "unworked" or "worked-unconfirmed".
	Status string `json:"status"`
	// ATNO marks an all-time new one.
	ATNO bool `json:"atno,omitempty"`
}

// ChaseExpedition is the expedition block of a row.
type ChaseExpedition struct {
	Call      string `json:"call"`
	ModeClass string `json:"mode_class,omitempty"`
	// Tier is "first", "second", "worked" or "unknown".
	Tier  string `json:"tier"`
	State string `json:"state,omitempty"`
}

// ChaseWorked is the worked-before block of a row.
type ChaseWorked struct {
	QSOs     int      `json:"qsos"`
	Bands    []string `json:"bands"`
	ThisBand bool     `json:"this_band"`
}

// ExpeditionTierFirst is the tier of an expedition slot that the
// operator needs.
const ExpeditionTierFirst = "first"

// ATNO reports whether a need of the row is an all-time new one.
func (r ChaseRow) ATNO() bool {
	for _, n := range r.Needs {
		if n.ATNO {
			return true
		}
	}
	return false
}

// FirstTierExpedition reports whether the row is a first-tier
// expedition slot.
func (r ChaseRow) FirstTierExpedition() bool {
	return r.Expedition != nil && r.Expedition.Tier == ExpeditionTierFirst
}

// State is the state that auto7 publishes on its host slot. The client
// adds the host and the ts.
type State struct {
	Enabled bool
	Band    string
	Mode    string
	// Task is the current task, for example "call XX1AA".
	Task string
	// StopReason is the reason for the last stop.
	StopReason string
	// Reduced names the reduced mode. Empty means full operation.
	Reduced string
}

// Candidate is one decoded station.
type Candidate struct {
	Call string
	Band string
	// Mode is FT8 or FT4.
	Mode string
	// LastMsg is the type of the last message, for example "CQ" or "RR73".
	LastMsg string
}

// QSO is one logged QSO from the QSOLogged packet of WSJT-X.
type QSO struct {
	Call    string
	Grid    string
	Band    string
	FreqHz  uint64
	Mode    string
	TimeOn  time.Time
	TimeOff time.Time
	RSTSent string
	RSTRcvd string
	MyCall  string
	MyGrid  string
}

// statePayload is the JSON of the own state.
type statePayload struct {
	TS         string `json:"ts"`
	Host       string `json:"host"`
	Enabled    bool   `json:"enabled"`
	Band       string `json:"band,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Task       string `json:"task,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`
	Reduced    string `json:"reduced,omitempty"`
}

// candidatesPayload is the JSON of the candidates topic.
type candidatesPayload struct {
	TS         string             `json:"ts"`
	Host       string             `json:"host"`
	Candidates []candidatePayload `json:"candidates"`
}

type candidatePayload struct {
	Call    string `json:"call"`
	Band    string `json:"band"`
	Mode    string `json:"mode"`
	LastMsg string `json:"last_msg"`
}

// qsoPayload is the JSON of the QSO topic.
type qsoPayload struct {
	TS      string `json:"ts"`
	Host    string `json:"host"`
	Call    string `json:"call"`
	Grid    string `json:"grid,omitempty"`
	Band    string `json:"band"`
	FreqHz  uint64 `json:"freq_hz"`
	Mode    string `json:"mode"`
	TimeOn  string `json:"time_on"`
	TimeOff string `json:"time_off"`
	RSTSent string `json:"rst_sent,omitempty"`
	RSTRcvd string `json:"rst_rcvd,omitempty"`
	MyCall  string `json:"my_call,omitempty"`
	MyGrid  string `json:"my_grid,omitempty"`
}
