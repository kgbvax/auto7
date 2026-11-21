// SPDX-License-Identifier: AGPL-3.0-or-later

package hunt

import (
	"context"
	"sync"
	"time"

	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/tx"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// Gate is the part of the TX gate that the task choice uses. The gate
// decides each request. The task choice only reads its state.
type Gate interface {
	Submit(ctx context.Context, r tx.Request) tx.Decision
	State() tx.State
}

// SpotHook looks for a spot of a high-value station on another listed
// band. When it finds one, it hands a band change to the gate and
// returns true. The band forager fills it. A nil hook finds no spot.
type SpotHook func(ctx context.Context, st tx.State) bool

// Heard is one decode of the current period: the joined station and the
// decode exactly as WSJT-X sent it. The gate repeats the decode in a
// Reply.
type Heard struct {
	Station Station
	Decode  wsjtx.Decode
}

// TaskKind names the task that the choice made for one period.
type TaskKind string

// Task kinds, in the order in which auto7 selects them.
const (
	// TaskNone means that auto7 has nothing to do in this period.
	TaskNone TaskKind = "none"
	// TaskQSO means that a QSO runs. auto7 sends no request.
	TaskQSO TaskKind = "QSO"
	// TaskHighValue means that auto7 hunts a high-value station on this
	// band. Callers wait.
	TaskHighValue TaskKind = "high-value hunt"
	// TaskSpot means that the spot hook started a band change to a
	// high-value spot on another band.
	TaskSpot TaskKind = "high-value spot"
	// TaskAnswer means that auto7 answers a caller.
	TaskAnswer TaskKind = "answer"
	// TaskHunt means that auto7 hunts a needed station.
	TaskHunt TaskKind = "hunt"
)

// Notes of a task without a request.
const (
	NoteStopped     = "auto7 is stopped"
	NoteWaitQSO     = "the station works another station"
	NoteNoStation   = "no station to call"
	NoteContinue    = "WSJT-X repeats the call"
	NoteSpotPending = "band change to a high-value spot"
)

// Task is the result of one period.
type Task struct {
	Kind TaskKind
	// Call is the station of the task, if any.
	Call string
	// Tries is the number of tries for Call after this period. It is 0
	// for a task that is not a hunt.
	Tries int
	// Requested is true when the choice handed a call or answer request
	// to the gate in this period. Decision is the answer of the gate.
	Requested bool
	Decision  tx.Decision
	// GaveUp lists the stations that the hunt gave up in this period.
	// They are on the skip list.
	GaveUp []string
	// Note says why the task has no request.
	Note string
}

// Highlight colors of a station that auto7 must not call: black on
// amber.
const (
	highlightBgR, highlightBgG, highlightBgB = 255, 191, 0
)

// OwnCall returns the own call from the last WSJT-X Status, in upper
// case. It is empty before the first Status.
func (n *Needs) OwnCall() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return normCall(n.status.DECall)
}

// Chooser selects the next task once per period and hands the requests
// to the gate. All methods are safe for use by many goroutines.
type Chooser struct {
	gate    Gate
	lists   *Lists
	limits  config.Limits
	ownCall func() string

	mu     sync.Mutex
	spot   SpotHook
	heard  []Heard
	period time.Duration
	// target is the station of the running hunt, or "". active is true
	// while WSJT-X calls the target.
	target   string
	targetHV bool
	active   bool
	// tries counts the tries per station.
	tries map[string]int
	// highlighted holds the stations that the gate highlighted on a
	// band.
	highlighted map[key]bool
	// cq is the own count of the CQ run. offset moves the TX offset, and
	// spotCheck finds a high-value spot on a listed band.
	cq        cqRun
	offset    OffsetHook
	spotCheck func() bool
	// answering is the caller of an accepted answer while the gate has
	// not opened the QSO view yet. answerLeft counts the periods that the
	// answer still holds.
	answering  string
	answerLeft int
}

// answerPeriods is the number of periods after an accepted answer that
// belong to the answer: the own TX period, which has no decode of the
// caller, and the period of the reply of the caller. In these periods
// auto7 sends no CQ, does not move its TX offset and does not change
// band, so WSJT-X keeps the answer.
const answerPeriods = 2

// NewChooser returns a Chooser. ownCall gives the own call, normally
// Needs.OwnCall, which reads the DE call of the last WSJT-X Status.
func NewChooser(gate Gate, lists *Lists, limits config.Limits, ownCall func() string) *Chooser {
	return &Chooser{
		gate: gate, lists: lists, limits: limits, ownCall: ownCall,
		tries: map[string]int{}, highlighted: map[key]bool{},
	}
}

// SetSpotHook sets the hook for a high-value spot on another band.
func (c *Chooser) SetSpotHook(h SpotHook) {
	c.mu.Lock()
	c.spot = h
	c.mu.Unlock()
}

// Add collects one decode for the next Run. A decode of a newer period
// drops the decodes of the older period, and a decode of an older period
// is dropped.
func (c *Chooser) Add(h Heard) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case len(c.heard) == 0 || h.Decode.Time > c.period:
		c.heard, c.period = []Heard{h}, h.Decode.Time
	case h.Decode.Time == c.period:
		c.heard = append(c.heard, h)
	}
}

// Run selects the task for the collected period and hands its requests
// to the gate. Call it once per period, after the last decode of the
// period. Each call and answer request carries the decode and its
// receive time, so the gate can refuse a request that is too late.
func (c *Chooser) Run(ctx context.Context) Task {
	c.mu.Lock()
	defer c.mu.Unlock()
	heard := c.heard
	c.heard = nil
	c.tickCQ()
	t := c.choose(ctx, heard)
	c.afterTask(c.gate.State(), t)
	return t
}

// choose applies the task order: a running QSO, a high-value hunt, a
// high-value spot on another band, a caller, a needed hunt, and last CQ.
// The caller holds mu.
func (c *Chooser) choose(ctx context.Context, heard []Heard) Task {
	own := normCall(c.ownCall())
	st := c.gate.State()
	if !st.Arm.Armed() {
		c.dropTarget()
		c.dropAnswer()
		return Task{Kind: TaskNone, Note: NoteStopped}
	}
	by := map[string]Heard{}
	for _, h := range heard {
		by[h.Station.Call] = h
	}
	c.highlight(ctx, heard)
	if st.QSO.Open {
		// The QSO ends any hunt. A new hunt of the partner starts with
		// new tries.
		delete(c.tries, normCall(st.QSO.DXCall))
		c.dropTarget()
		c.dropAnswer()
		return Task{Kind: TaskQSO, Call: normCall(st.QSO.DXCall)}
	}
	if c.answering != "" {
		// An accepted answer holds until the QSO view opens. It ends
		// early when the caller works another station.
		h, heard := by[c.answering]
		if c.answerLeft > 0 && !(heard && inOtherQSO(h.Station, own)) {
			c.answerLeft--
			return Task{Kind: TaskAnswer, Call: c.answering, Note: NoteContinue}
		}
		c.dropAnswer()
	}

	var t Task
	// stepped is the station that the hunt already handled in this
	// period. It gets no second request.
	stepped := ""
	// A running high-value hunt continues, and callers wait.
	if c.target != "" && c.targetHV {
		stepped = c.target
		if c.step(ctx, by, own, &t) {
			t.Kind = TaskHighValue
			return t
		}
	}
	hv := c.candidates(heard, own, true)
	if h, ok := best(hv, func(h Heard) bool { return callable(h.Station, own) && h.Station.Call != stepped }); ok {
		c.start(h, true)
		if c.step(ctx, by, own, &t) {
			t.Kind = TaskHighValue
			return t
		}
	}
	if len(hv) > 0 {
		// A high-value station works another station. auto7 waits for
		// it and stops a hunt of a station that is not high value.
		c.pause(ctx)
		t.Kind, t.Call, t.Note = TaskHighValue, hv[0].Station.Call, NoteWaitQSO
		return t
	}

	if c.spot != nil && c.spot(ctx, st) {
		c.dropTarget()
		t.Kind, t.Note = TaskSpot, NoteSpotPending
		return t
	}

	callers := make([]Heard, 0, len(heard))
	for _, h := range heard {
		if isCaller(h.Station, own) && !h.Station.HighlightOnly {
			callers = append(callers, h)
		}
	}
	if h, ok := best(callers, nil); ok {
		// The answer replaces the call of a running hunt in WSJT-X.
		c.dropTarget()
		d := c.gate.Submit(ctx, tx.Answer{Decode: h.Decode, Heard: h.Station.HeardAt})
		t.Kind, t.Call, t.Requested, t.Decision = TaskAnswer, h.Station.Call, true, d
		if d.OK {
			c.answering, c.answerLeft = h.Station.Call, answerPeriods
		}
		return t
	}

	if c.target != "" && !c.targetHV {
		stepped = c.target
		if c.step(ctx, by, own, &t) {
			t.Kind = TaskHunt
			return t
		}
	}
	needed := c.candidates(heard, own, false)
	if h, ok := best(needed, func(h Heard) bool { return callable(h.Station, own) && h.Station.Call != stepped }); ok {
		c.start(h, false)
		if c.step(ctx, by, own, &t) {
			t.Kind = TaskHunt
			return t
		}
	}

	if len(needed) > 0 {
		// A needed station works another station. auto7 waits for it.
		t.Kind, t.Note = TaskNone, NoteNoStation
		return t
	}
	return c.chooseCQ(ctx, st, t)
}

// step runs one period of the hunt of the target. It returns true when
// the hunt goes on in this period. It returns false when the hunt gave
// up the target, or when the target cannot be called now. The caller
// holds mu.
func (c *Chooser) step(ctx context.Context, by map[string]Heard, own string, t *Task) bool {
	call := c.target
	h, heard := by[call]
	if heard && inOtherQSO(h.Station, own) {
		// Never call into an open QSO of the target. The tries stay, and
		// the hunt resumes when the target ends the QSO.
		c.pause(ctx)
		return false
	}
	if c.tries[call] >= triesLimit(c.limits, c.targetHV) {
		if c.active {
			c.gate.Submit(ctx, tx.GiveUp{Call: call})
		}
		if c.lists.Skip(call) == nil {
			delete(c.tries, call)
		}
		c.dropTarget()
		t.GaveUp = append(t.GaveUp, call)
		return false
	}
	t.Call = call
	if heard && callable(h.Station, own) {
		d := c.gate.Submit(ctx, tx.Call{Decode: h.Decode, Heard: h.Station.HeardAt})
		t.Requested, t.Decision = true, d
		if d.OK {
			c.active = true
			c.tries[call]++
			c.dropAnswer()
		}
		t.Tries = c.tries[call]
		return d.OK || c.active
	}
	if c.active {
		// WSJT-X repeats the call in this period. It counts as a try.
		c.tries[call]++
		t.Tries, t.Note = c.tries[call], NoteContinue
		return true
	}
	return false
}

// start makes h the target of the hunt. The caller holds mu.
func (c *Chooser) start(h Heard, highValue bool) {
	if c.target != h.Station.Call {
		c.target, c.targetHV, c.active = h.Station.Call, highValue, false
	}
}

// pause ends the calls of an active hunt with a give-up request and
// keeps the tries. The caller holds mu.
func (c *Chooser) pause(ctx context.Context) {
	if c.target != "" && c.active {
		c.gate.Submit(ctx, tx.GiveUp{Call: c.target})
	}
	c.active = false
}

// dropTarget forgets the target and keeps its tries. The caller holds mu.
func (c *Chooser) dropTarget() {
	c.target, c.targetHV, c.active = "", false, false
}

// dropAnswer ends the hold of an accepted answer. The caller holds mu.
func (c *Chooser) dropAnswer() {
	c.answering, c.answerLeft = "", 0
}

// candidates returns the stations that auto7 may hunt: needed, not
// worked, not skipped, not highlight only. highValue selects the
// high-value stations, else the other needed stations. The caller holds
// mu.
func (c *Chooser) candidates(heard []Heard, own string, highValue bool) []Heard {
	var out []Heard
	for _, h := range heard {
		s := h.Station
		if !s.Needed || s.HighValue != highValue || s.Worked || s.HighlightOnly || s.Call == own {
			continue
		}
		if c.lists.Skipped(s.Call) {
			continue
		}
		out = append(out, h)
	}
	return out
}

// best returns the station with the strongest signal among the ones that
// ok accepts. A nil ok accepts all.
func best(hs []Heard, ok func(Heard) bool) (Heard, bool) {
	var out Heard
	found := false
	for _, h := range hs {
		if ok != nil && !ok(h) {
			continue
		}
		if !found || h.Station.SNR > out.Station.SNR {
			out, found = h, true
		}
	}
	return out, found
}

// highlight sends a highlight request for each needed station that auto7
// must not call, once per station and band. The caller holds mu.
func (c *Chooser) highlight(ctx context.Context, heard []Heard) {
	for _, h := range heard {
		s := h.Station
		k := key{s.Call, s.Band}
		if !s.HighlightOnly || !s.Needed || c.highlighted[k] {
			continue
		}
		r := tx.Highlight{
			Call: s.Call,
			Bg:   tx.RGB(highlightBgR, highlightBgG, highlightBgB),
			Fg:   tx.RGB(0, 0, 0),
		}
		if c.gate.Submit(ctx, r).OK {
			c.highlighted[k] = true
		}
	}
}
