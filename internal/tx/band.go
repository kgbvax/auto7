// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"slices"
	"time"

	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/tx/send"
)

// bandDials holds the default FT8 and FT4 dials of WSJT-X for IARU
// region 1, from models/FrequencyList.cpp of WSJT-X. 0 means that WSJT-X
// has no default dial for the mode on the band.
var bandDials = map[string]struct{ ft8, ft4 uint64 }{
	"160m": {1_840_000, 0},
	"80m":  {3_573_000, 3_575_000},
	"40m":  {7_074_000, 7_047_500},
	"30m":  {10_136_000, 10_140_000},
	"20m":  {14_074_000, 14_080_000},
	"17m":  {18_100_000, 18_104_000},
	"15m":  {21_074_000, 21_140_000},
	"12m":  {24_915_000, 24_919_000},
	"10m":  {28_074_000, 28_180_000},
	"6m":   {50_313_000, 50_318_000},
}

// bandEdges holds the edges of the bands in Hz, for IARU region 1 and
// wide enough for the other regions.
var bandEdges = []struct {
	name     string
	low, top uint64
}{
	{"160m", 1_800_000, 2_000_000},
	{"80m", 3_500_000, 4_000_000},
	{"40m", 7_000_000, 7_300_000},
	{"30m", 10_100_000, 10_150_000},
	{"20m", 14_000_000, 14_350_000},
	{"17m", 18_068_000, 18_168_000},
	{"15m", 21_000_000, 21_450_000},
	{"12m", 24_890_000, 24_990_000},
	{"10m", 28_000_000, 29_700_000},
	{"6m", 50_000_000, 54_000_000},
}

// DefaultDial gives the default WSJT-X dial in Hz of mode on band, for
// example 14074000 for FT8 on 20m. ok is false when WSJT-X has no
// default dial for them. 60m is not in the table, because auto7 has no
// power limit for it.
func DefaultDial(band, mode string) (hz uint64, ok bool) {
	d, ok := bandDials[band]
	if !ok {
		return 0, false
	}
	switch mode {
	case config.ModeFT8:
		hz = d.ft8
	case config.ModeFT4:
		hz = d.ft4
	}
	return hz, hz != 0
}

// BandOf gives the band of a dial in Hz, for example "20m". It is empty
// when the dial is on no band of the table.
func BandOf(hz uint64) string {
	for _, b := range bandEdges {
		if hz >= b.low && hz <= b.top {
			return b.name
		}
	}
	return ""
}

// Phases of a band change.
const (
	// bandHalting waits for a Status with Enable Tx off and Transmitting
	// false after an own HaltTx.
	bandHalting = iota
	// bandSettling waits for WSJT-X to report the new dial and mode
	// after the dial change.
	bandSettling
)

// bandChange is the band change that the gate carries out.
type bandChange struct {
	band, mode string
	dial       uint64
	phase      int
	// deadline ends the halting phase.
	deadline time.Time
	// haltSeq is the Status count at the own HaltTx. The halting phase
	// ends only with a later Status.
	haltSeq uint64
	// sent is the time of SetDialFrequency.
	sent time.Time
	// seen is the time of the first Status with the new dial and mode
	// and Transmitting false. It is zero while no such Status holds.
	seen time.Time
}

// listed reports whether the band list of the configuration holds band
// with mode.
func (c *core) listed(band, mode string) bool {
	for _, b := range c.bands {
		if b.Name == band {
			return slices.Contains(b.Modes, mode)
		}
	}
	return false
}

// wsjtxTransmits reports whether the last WSJT-X Status shows
// Transmitting true.
func (c *core) wsjtxTransmits() bool {
	return c.in.hasStatus && c.in.status.Transmitting
}

// changeBand decides a band change request. The caller checked the arm
// state and the build.
func (c *core) changeBand(now time.Time, r BandChange) Decision {
	dial, ok := DefaultDial(r.Band, r.Mode)
	if !ok {
		return refused(ReasonBandUnknown)
	}
	if !c.listed(r.Band, r.Mode) {
		return refused(ReasonBandNotListed)
	}
	if c.qso.view.Open {
		return refused(ReasonQSORuns)
	}
	if !c.in.hasStatus {
		return refused(ReasonNoStatus)
	}
	if c.wsjtxTransmits() {
		return refused(ReasonTransmits)
	}
	c.band = &bandChange{band: r.Band, mode: r.Mode, dial: dial, phase: bandHalting}
	// A Reply or a CQ waits no more for its check, because the band
	// change ends it.
	c.reply = nil
	c.giveUpAt = time.Time{}
	c.cq.active = false
	// WSJT-X keeps Enable Tx on across a dial change. Without this step
	// it can transmit on the new band in the next slot. The gate halts
	// also when the last Status shows Enable Tx off, because an own
	// Reply or CQ can turn it on after that Status.
	c.band.deadline = now.Add(2 * c.period())
	c.band.haltSeq = c.statusSeq
	c.halt(true)
	return accepted
}

// bandProgress moves a band change on from its halting phase.
func (c *core) bandProgress(now time.Time) {
	b := c.band
	if b == nil || b.phase != bandHalting || c.arm != Running {
		return
	}
	switch {
	case c.qso.view.Open:
		c.band = nil
		c.refuse(now, kindOf(BandChange{}), ReasonQSORuns)
	case c.statusSeq > b.haltSeq && !c.in.status.TxEnabled && !c.wsjtxTransmits():
		c.startDial(now)
	case !now.Before(b.deadline):
		c.band = nil
		c.refuse(now, kindOf(BandChange{}), ReasonNoHalt)
	}
}

// startDial enters Settling, records the dial and the mode, and sends
// SetDialFrequency and then Configure. SetDialFrequency goes first, so
// the dial is already in the frequency table of the mode, and Configure
// does not move the rig again.
func (c *core) startDial(now time.Time) {
	b := c.band
	c.arm = Settling
	b.phase, b.sent = bandSettling, now
	c.bandAt = now
	c.rec.add(now, fieldDial, b.dial, "")
	c.rec.add(now, fieldMode, 0, b.mode)
	c.log.Info("tx: band change", "band", b.band, "mode", b.mode, "dial_hz", b.dial)
	if err := c.send.SetDialFrequency(b.dial); err != nil {
		c.log.Error("tx: SetDialFrequency failed", "err", err)
		// The dial change did not go out, so the next Enable does not
		// wait for it. bandAt stays, so old decodes stay refused.
		c.band = nil
		c.stop(now, ReasonSendFailed)
		return
	}
	if err := c.send.Configure(send.ModeOnly(b.mode)); err != nil {
		c.log.Error("tx: Configure failed", "err", err)
		// Without the mode, WSJT-X never reports the expected dial and
		// mode. The next Enable runs on the current dial.
		c.band = nil
		c.stop(now, ReasonSendFailed)
	}
}
