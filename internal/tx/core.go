// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"errors"
	"log/slog"
	"net/netip"
	"time"

	"github.com/kgbvax/auto7/internal/bus"
	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/tx/send"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// Timing rules of the gate.
const (
	// SlotLateness is how long after the start of a slot the gate still
	// sends a Reply for that slot.
	SlotLateness = 2 * time.Second
	// RepublishEvery is the interval of the state republish while auto7 is
	// enabled. It keeps the ts fresh for the second controller check of
	// other hosts.
	RepublishEvery = 30 * time.Second
	// MinTxDF and MaxTxDF bound the TX offset that ws-hunter accepts.
	MinTxDF = 200
	MaxTxDF = 4000
)

// Periods of the modes.
const (
	periodFT8 = 15 * time.Second
	periodFT4 = 7500 * time.Millisecond
)

// reducedText is the reduced mode in the published state.
const reducedText = "stock WSJT-X"

// limits are the limits that the gate applies. The hard limits of the
// gate win over the configured values.
type limits struct {
	cqMax        int
	cqListen     int
	movesPerHour int
	minDistHz    uint32
}

func gateLimits(l config.Limits) limits {
	return limits{
		cqMax:        min(l.CQMaxPeriods, config.CQMaxPeriodsCeiling),
		cqListen:     max(l.CQListenPeriods, config.CQListenPeriodsFloor),
		movesPerHour: max(0, min(l.OffsetMovesPerHour, config.OffsetMovesPerHourCeiling)),
		minDistHz:    uint32(max(l.OffsetMinDistanceHz, config.OffsetDistanceFloorHz)),
	}
}

type coreOptions struct {
	Config    config.Config
	Conn      send.Conn
	Peer      func() (netip.AddrPort, bool)
	ResetPeer func()
	Now       func() time.Time
	Publish   func(bus.State)
	Logger    *slog.Logger
	// Task gives the current task of the hunt for the published state.
	// It can be nil.
	Task func() string
	// ListsErr reports an error while the hunt lists cannot be read. It
	// can be nil.
	ListsErr func() error
}

// pendingReply is the check window after a Reply.
type pendingReply struct {
	df         uint32
	deadline   time.Time
	sawEnabled bool
}

// core holds the gate state and decides every request. It has no
// goroutine and no lock. The Gate runs it on its one owner goroutine.
type core struct {
	send      *send.Sender
	now       func() time.Time
	publishFn func(bus.State)
	resetPeer func()
	log       *slog.Logger
	lim       limits
	in        *inputs
	bands     []config.Band
	taskFn    func() string
	listsErr  func() error

	// rec is the record of own commands.
	rec record
	// qso is the QSO view.
	qso qsoTracker
	// band is the band change that runs. It is nil when none runs.
	band *bandChange
	// statusSeq counts the WSJT-X Status messages. A band change waits
	// for a Status that came after its own HaltTx.
	statusSeq uint64
	// bandAt is the time of the last band change. The gate drops every
	// decode from before it.
	bandAt time.Time
	// hint names a likely cause of the last stop.
	hint string

	arm         ArmState
	stopReason  string
	stoppedAt   time.Time
	notStopped  bool
	lastRefusal Refusal
	confirm     struct {
		pending  bool
		repeated bool
		deadline time.Time
	}

	reply        *pendingReply
	giveUpAt     time.Time
	transmitting bool
	cq           struct {
		active      bool
		periods     int
		sent        int
		listenUntil time.Time
		// open is true from an own CQ until the next own Reply.
		open bool
	}
	moves []time.Time

	pub struct {
		has  bool
		last bus.State
		at   time.Time
	}
}

func newCore(o coreOptions) *core {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &core{
		send:      send.New(o.Conn, o.Config.WSJTX.ID, o.Peer),
		now:       now,
		publishFn: o.Publish,
		resetPeer: o.ResetPeer,
		log:       log,
		lim:       gateLimits(o.Config.Limits),
		in:        newInputs(o.Config.Host, o.Config.Presence.Hosts),
		bands:     o.Config.Bands,
		taskFn:    o.Task,
		listsErr:  o.ListsErr,
	}
}

// after runs the timers and the publish after each input.
func (c *core) after(now time.Time) {
	c.evaluate(now)
	c.maybePublish(now)
}

// evaluate applies the stops and runs the timers.
func (c *core) evaluate(now time.Time) {
	if c.arm.Armed() {
		if r := c.in.blocker(now); r != "" {
			c.stop(now, r)
		}
	}
	c.bandProgress(now)
	if c.arm == Settling {
		switch {
		case c.settled(now):
			c.arm = Running
			if c.band != nil {
				c.log.Info("tx: band change done", "band", c.band.band, "mode", c.band.mode)
				c.band = nil
			}
		case c.dialTimedOut(now):
			c.log.Warn("tx: "+ReasonNoDialChange, "band", c.band.band, "mode", c.band.mode, "dial_hz", c.band.dial)
			// WSJT-X did not reach the dial, so the next Enable does not
			// wait for it again. bandAt stays, so old decodes stay refused.
			c.band = nil
			c.stop(now, ReasonNoDialChange)
		}
	}
	c.qsoTimers(now)
	if c.confirm.pending && !c.confirm.repeated && !now.Before(c.confirm.deadline) {
		c.confirm.repeated = true
		c.notStopped = true
		c.log.Warn("tx: " + ReasonNotStopped + ": HaltTx again")
		c.halt(false)
	}
	if !c.giveUpAt.IsZero() && !now.Before(c.giveUpAt) {
		c.giveUpAt = time.Time{}
		if c.arm == Running {
			c.halt(true)
		}
	}
	if c.reply != nil && !now.Before(c.reply.deadline) {
		r := c.reply
		c.reply = nil
		if !r.sawEnabled {
			c.stop(now, ReasonAutoTxOff)
		}
	}
}

// stop latches a stop. It sends HaltTx at once and starts the wait for a
// Status that confirms the stop. A stop while stopped does nothing.
func (c *core) stop(now time.Time, reason string) {
	if !c.arm.Armed() {
		return
	}
	c.arm = Stopped
	c.stopReason = reason
	c.stoppedAt = now
	c.hint = ""
	// A dial change that went out stays expected, so that the next
	// Enable waits for the new dial and the settle time again. The caller
	// clears c.band for a dial change that failed.
	if c.band != nil && c.band.phase != bandSettling {
		c.band = nil
	}
	c.notStopped = false
	c.reply = nil
	c.giveUpAt = time.Time{}
	c.cq.active = false
	c.confirm.pending, c.confirm.repeated = true, false
	c.confirm.deadline = now.Add(c.period())
	c.log.Warn("tx: stop", "reason", reason)
	c.halt(false)
}

// halt sends HaltTx. With autoOnly false WSJT-X stops at once. The
// record holds the Enable Tx off that follows.
func (c *core) halt(autoOnly bool) {
	c.rec.add(c.now(), fieldTxEnabled, 0, "")
	if err := c.send.HaltTx(autoOnly); err != nil {
		c.log.Error("tx: HaltTx failed", "auto_only", autoOnly, "err", err)
	}
}

// period gives the T/R period of the current WSJT-X mode. Without a
// Status it gives the FT8 period.
func (c *core) period() time.Duration {
	st := c.in.status
	switch {
	case st.Mode == config.ModeFT4:
		return periodFT4
	case st.Mode == config.ModeFT8:
		return periodFT8
	case c.in.hasStatus && st.TRPeriod != wsjtx.NotApplicable && st.TRPeriod > 0:
		return time.Duration(st.TRPeriod) * time.Second
	}
	return periodFT8
}

// midnight gives the start of the UTC day of t.
func midnight(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// slotStart gives the start of the slot that holds now.
func slotStart(now time.Time, p time.Duration) time.Time {
	day := midnight(now)
	off := now.Sub(day)
	return day.Add(off - off%p)
}

// handleWSJTX takes one event of the WSJT-X receiver.
func (c *core) handleWSJTX(e wsjtx.Event) {
	now := c.now()
	if e.Kind == wsjtx.EventPeerChanged {
		// A new WSJT-X address is a stale WSJT-X input. auto7 learns the
		// new address only while it is stopped.
		if c.arm.Armed() {
			c.stop(now, ReasonPeerChanged)
		} else if c.resetPeer != nil {
			c.resetPeer()
		}
		c.after(now)
		return
	}
	c.checkRestart(now, e.Msg)
	prev, hadPrev := c.in.status, c.in.hasStatus
	c.in.handleWSJTX(e)
	at := e.At
	if at.IsZero() {
		at = now
	}
	switch m := e.Msg.(type) {
	case wsjtx.Status:
		c.statusSeq++
		if c.txInBandChange(m) {
			c.log.Warn("tx: "+ReasonTxInBandChange, "dial_hz", m.DialHz)
			c.stop(now, ReasonTxInBandChange)
			c.after(now)
			return
		}
		c.onStatus(now, m)
		if hadPrev {
			c.checkTakeover(now, prev, m)
		}
		c.settleStatus(now, m)
		c.cqRising(now, m)
		c.qsoStatus(now, m)
	case wsjtx.Decode:
		c.qsoDecode(at, m)
	case wsjtx.QSOLogged:
		c.qsoLogged()
		// WSJT-X can clear the DX call after it logged the QSO.
		c.rec.add(now, fieldDXCall, 0, "")
	}
	c.after(now)
}

// checkRestart stops when WSJT-X closes, or when a Heartbeat shows
// another version or revision than the Heartbeat before.
func (c *core) checkRestart(now time.Time, m wsjtx.Message) {
	switch m := m.(type) {
	case wsjtx.Close:
		c.stop(now, ReasonRestarted)
	case wsjtx.Heartbeat:
		if c.in.hbSeen && (m.Version != c.in.hbVersion || m.Revision != c.in.hbRevision) {
			c.stop(now, ReasonRestarted)
		}
	}
}

func absDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

// onStatus applies the checks that each Status drives.
func (c *core) onStatus(now time.Time, st wsjtx.Status) {
	if c.confirm.pending && !st.Transmitting && !st.TxEnabled {
		c.confirm.pending = false
		c.notStopped = false
	}
	if c.reply != nil && c.arm.Armed() {
		if st.TxEnabled {
			c.reply.sawEnabled = true
		}
		if absDiff(st.TxDF, c.reply.df) < c.lim.minDistHz {
			c.stop(now, ReasonHoldOff)
		}
	}
}

// cqRising counts the CQ transmissions of a CQ run and ends the run.
func (c *core) cqRising(now time.Time, st wsjtx.Status) {
	rising := st.Transmitting && !c.transmitting
	c.transmitting = st.Transmitting
	if rising && c.cq.active && c.arm == Running {
		c.cq.sent++
		if c.cq.sent >= c.cq.periods {
			c.cq.active = false
			c.cq.listenUntil = now.Add(time.Duration(c.lim.cqListen+1) * c.period())
			c.halt(true)
		}
	}
}

// handleBus takes one event of the bus client.
func (c *core) handleBus(e bus.Event) {
	now := c.now()
	c.in.handleBus(e)
	switch e := e.(type) {
	case bus.Link:
		if e.Up {
			// A new connection publishes the state again.
			c.pub.has = false
		}
	case bus.StopCommand:
		c.stopOrRepeat(now, ReasonBusCommand)
	}
	c.after(now)
}

// tick runs the timers.
func (c *core) tick() { c.after(c.now()) }

// refuse records a refusal.
func (c *core) refuse(now time.Time, kind, reason string) {
	c.lastRefusal = Refusal{Kind: kind, Reason: reason, At: now}
	c.log.Info("tx: refused", "request", kind, "reason", reason)
}

// enable arms the gate. It returns the reason when it refuses.
func (c *core) enable() error {
	now := c.now()
	c.evaluate(now)
	if c.arm.Armed() {
		return nil
	}
	refuse := func(reason string) error {
		c.refuse(now, "enable", reason)
		c.after(now)
		return errors.New(reason)
	}
	if c.listsErr != nil && c.listsErr() != nil {
		return refuse(ReasonListsUnreadable)
	}
	if r := c.in.blocker(now); r != "" {
		return refuse(r)
	}
	if !c.send.HasPeer() {
		return refuse(ReasonNoPeerAddr)
	}
	if c.in.build == wsjtx.BuildFull {
		// With Hold Tx Freq on, a Reply does not move the own TX offset
		// to the offset of the called station.
		if err := c.send.SetHoldTxFreq(true); err != nil {
			c.log.Error("tx: SetHoldTxFreq failed", "err", err)
			return refuse(ReasonSendFailed)
		}
	}
	c.arm = Settling
	c.stopReason = ""
	c.stoppedAt = time.Time{}
	c.notStopped = false
	c.confirm.pending, c.confirm.repeated = false, false
	c.cq.active, c.cq.listenUntil = false, time.Time{}
	c.reply = nil
	c.giveUpAt = time.Time{}
	c.cq.open = false
	c.resumeSettle(now)
	c.hint = ""
	c.log.Info("tx: enabled", "build", c.in.build.String())
	c.after(now)
	return nil
}

// operatorStop stops on a request of the operator.
func (c *core) operatorStop(reason string) {
	now := c.now()
	c.stopOrRepeat(now, reason)
	c.after(now)
}

// stopOrRepeat stops an armed gate. While a stop waits for its
// confirmation, it sends HaltTx again and starts a new confirm wait.
func (c *core) stopOrRepeat(now time.Time, reason string) {
	if c.arm.Armed() || !c.confirm.pending {
		c.stop(now, reason)
		return
	}
	c.log.Warn("tx: stop again while not confirmed: HaltTx again", "reason", reason)
	c.confirm.repeated = false
	c.confirm.deadline = now.Add(c.period())
	c.halt(false)
}

// shutdown runs before auto7 exits. It stops an armed gate, and it sends
// HaltTx again while a stop waits for its confirmation.
func (c *core) shutdown() {
	now := c.now()
	switch {
	case c.arm.Armed():
		c.stop(now, ReasonExit)
	case c.confirm.pending:
		c.halt(false)
	}
}

func kindOf(r Request) string {
	switch r.(type) {
	case Call:
		return "call"
	case Answer:
		return "answer"
	case GiveUp:
		return "give-up"
	case CQ:
		return "CQ"
	case OffsetMove:
		return "offset move"
	case BandChange:
		return "band change"
	case Highlight:
		return "highlight"
	}
	return "unknown"
}

var accepted = Decision{OK: true}

func refused(reason string) Decision { return Decision{Reason: reason} }

// submit decides one request.
func (c *core) submit(r Request) Decision {
	now := c.now()
	c.evaluate(now)
	d := c.decide(now, r)
	if !d.OK {
		c.refuse(now, kindOf(r), d.Reason)
	}
	c.after(now)
	return d
}

func (c *core) decide(now time.Time, r Request) Decision {
	switch c.arm {
	case Stopped:
		return refused(ReasonStopped)
	case Settling:
		return refused(ReasonSettling)
	}
	if _, ok := r.(Highlight); c.band != nil && !ok {
		return refused(ReasonBandChanging)
	}
	full := c.in.build == wsjtx.BuildFull
	switch r := r.(type) {
	case Call:
		if reason := c.mayCall(r.Decode); reason != "" {
			return refused(reason)
		}
		if !c.in.chaseFresh(now) {
			return refused(ReasonChaseStale)
		}
		return c.sendReply(now, r.Decode, r.Heard)
	case Answer:
		if reason := c.mayCall(r.Decode); reason != "" {
			return refused(reason)
		}
		return c.sendReply(now, r.Decode, r.Heard)
	case GiveUp:
		c.giveUpAt = c.giveUpDeadline(now)
		return accepted
	case CQ:
		if !full {
			return refused(ReasonReduced)
		}
		if reason := c.mayCall(wsjtx.Decode{}); reason != "" {
			return refused(reason)
		}
		return c.startCQ(now, r)
	case OffsetMove:
		if !full {
			return refused(ReasonReduced)
		}
		return c.moveOffset(now, r)
	case BandChange:
		if !full {
			// Stock WSJT-X has no UDP command for the dial.
			return refused(ReasonReduced)
		}
		return c.changeBand(now, r)
	case Highlight:
		if err := c.send.HighlightCallsign(r.Call, r.Bg, r.Fg, r.Last); err != nil {
			return refused(ReasonSendFailed)
		}
		return accepted
	}
	return refused("unknown request")
}

// mayCall applies the rules of every call, answer and CQ: no call in a
// special operating mode, and no call that stops a running QSO. d is the
// decode of the target. A CQ has no target.
func (c *core) mayCall(d wsjtx.Decode) string {
	if c.in.hasStatus && c.in.status.SpecialOp != wsjtx.SpecialOpNone {
		return ReasonSpecialOp
	}
	if c.qso.view.Open {
		if _, from := calls(d.Message); from == "" || from != c.qso.view.DXCall {
			return ReasonQSORuns
		}
	}
	return ""
}

// late reports whether a Reply to the decode d comes too late. A decode
// older than one period is late. A Reply later than SlotLateness after
// the start of the slot after the decode is also late.
func late(now time.Time, d wsjtx.Decode, heard time.Time, p time.Duration) bool {
	if heard.IsZero() || now.Sub(heard) > p || d.Time < 0 {
		return true
	}
	return now.After(decodeStart(now, d).Add(p).Add(SlotLateness))
}

// sendReply applies the timing rule and the offset guard, then sends the
// Reply.
func (c *core) sendReply(now time.Time, d wsjtx.Decode, heard time.Time) Decision {
	if !c.in.hasStatus {
		return refused(ReasonNoStatus)
	}
	if !c.bandAt.IsZero() && (heard.Before(c.bandAt) || d.Time < 0 || decodeStart(now, d).Before(c.bandAt)) {
		return refused(ReasonOldDecode)
	}
	p := c.period()
	if late(now, d, heard, p) {
		return refused(ReasonTooLate)
	}
	if absDiff(c.in.status.TxDF, d.DeltaFreq) < c.lim.minDistHz {
		return refused(ReasonTooClose)
	}
	if err := c.send.Reply(d); err != nil {
		c.log.Error("tx: Reply failed", "err", err)
		return refused(ReasonSendFailed)
	}
	// WSJT-X sets the DX call to the sender of the decode and turns
	// Enable Tx on.
	_, from := calls(d.Message)
	c.rec.add(now, fieldDXCall, 0, from)
	c.rec.add(now, fieldTxEnabled, 1, "")
	c.reply = &pendingReply{df: d.DeltaFreq, deadline: now.Add(p)}
	c.giveUpAt = time.Time{}
	c.cq.active = false
	c.cq.open = false
	return accepted
}

// giveUpDeadline gives the last time at which a Reply for the next slot
// can still go out.
func (c *core) giveUpDeadline(now time.Time) time.Time {
	p := c.period()
	start := slotStart(now, p)
	if d := start.Add(SlotLateness); !now.After(d) {
		return d
	}
	return start.Add(p).Add(SlotLateness)
}

// startCQ starts a CQ run after the hard limits.
func (c *core) startCQ(now time.Time, r CQ) Decision {
	if !c.in.chaseFresh(now) {
		return refused(ReasonChaseStale)
	}
	if r.Periods < 1 || r.Periods > c.lim.cqMax {
		return refused(ReasonCQLimit)
	}
	if c.cq.active {
		return refused(ReasonCQActive)
	}
	if now.Before(c.cq.listenUntil) {
		return refused(ReasonCQListen)
	}
	if r.Directed {
		st := c.in.status
		if !c.in.hasStatus || st.DECall == "" || len(st.DEGrid) < 4 {
			return refused(ReasonNoOwnCall)
		}
		if err := c.send.FreeText("CQ DX "+st.DECall+" "+st.DEGrid[:4], true); err != nil {
			return refused(ReasonSendFailed)
		}
	} else {
		c.rec.add(now, fieldDXCall, 0, "")
		if err := c.send.ClearDX(); err != nil {
			return refused(ReasonSendFailed)
		}
	}
	c.rec.add(now, fieldTxEnabled, 1, "")
	if err := c.send.SetTxEnabled(true); err != nil {
		return refused(ReasonSendFailed)
	}
	c.cq.active, c.cq.periods, c.cq.sent = true, r.Periods, 0
	c.cq.open = true
	// A give-up before the CQ must not halt the CQ.
	c.giveUpAt = time.Time{}
	return accepted
}

// moveOffset moves the TX offset after the hard limit per hour.
func (c *core) moveOffset(now time.Time, r OffsetMove) Decision {
	if r.TxDF < MinTxDF || r.TxDF > MaxTxDF {
		return refused(ReasonOffsetRange)
	}
	kept := c.moves[:0]
	for _, t := range c.moves {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	c.moves = kept
	if len(c.moves) >= c.lim.movesPerHour {
		return refused(ReasonOffsetLimit)
	}
	c.rec.add(now, fieldTxDF, uint64(r.TxDF), "")
	if err := c.send.SetTxDF(r.TxDF); err != nil {
		return refused(ReasonSendFailed)
	}
	c.moves = append(c.moves, now)
	return accepted
}

// busState gives the state to publish on the own host slot.
func (c *core) busState() bus.State {
	s := bus.State{
		Enabled:    c.arm.Armed(),
		Band:       BandOf(c.in.status.DialHz),
		Mode:       c.in.status.Mode,
		StopReason: c.stopReason,
	}
	if c.in.build != wsjtx.BuildFull {
		s.Reduced = reducedText
	}
	if c.taskFn != nil {
		s.Task = c.taskFn()
	}
	return s
}

// maybePublish publishes the state when it changed, after a new
// connection, and every RepublishEvery while auto7 is enabled.
func (c *core) maybePublish(now time.Time) {
	if c.publishFn == nil || !c.in.linkUp {
		return
	}
	s := c.busState()
	if c.pub.has && s == c.pub.last && !(s.Enabled && now.Sub(c.pub.at) >= RepublishEvery) {
		return
	}
	c.pub.has, c.pub.last, c.pub.at = true, s, now
	c.publishFn(s)
}

// snapshot gives the state for the other packages.
func (c *core) snapshot() State {
	now := c.now()
	host, _ := c.in.presence.host(now)
	return State{
		Arm:          c.arm,
		StopReason:   c.stopReason,
		StoppedAt:    c.stoppedAt,
		NotStopped:   c.notStopped,
		Build:        c.in.build,
		PresenceHost: host,
		ChaseStale:   !c.in.chaseFresh(now),
		Band:         BandOf(c.in.status.DialHz),
		Mode:         c.in.status.Mode,
		LastRefusal:  c.lastRefusal,
		QSO:          c.qso.view,
		Hint:         c.hint,
		Warning:      c.qso.warning,
	}
}
