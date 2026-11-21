// SPDX-License-Identifier: AGPL-3.0-or-later

package bands

import (
	"context"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/kgbvax/auto7/internal/config"
	"github.com/kgbvax/auto7/internal/tx"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// Rules of the offset choice.
const (
	// keepPeriods is the number of the newest periods whose decodes and
	// spectra mark occupied offsets. Two periods hold both slots: the
	// stations of the even and of the odd slot.
	keepPeriods = 2
	// signalMarginDB is the level above the noise at which a spectrum
	// bin holds a signal.
	signalMarginDB = 6
	// noisePercentile is the share of the bins that lies below the noise
	// floor estimate. Most bins of a passband hold only noise.
	noisePercentile = 0.2
	// retryAfter is the wait after a refused hop request.
	retryAfter = time.Minute
)

// OffsetOptions holds the inputs of an Offsets.
type OffsetOptions struct {
	Gate Gate
	// Limits gives the offset moves per hour and the offset distance. The
	// hard limits of the gate cap them.
	Limits config.Limits
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Rand gives a random number from 0 to n-1. Nil means math/rand.
	Rand func(n int) int
	// Logger receives the log lines. Nil means slog.Default.
	Logger *slog.Logger
}

// Offsets chooses the own TX offset at a clear place and hands offset
// moves to the gate. It maps the occupied offsets of the newest periods
// from the decodes and, with the ws-hunter build, from the spectrum. All
// methods are safe for use by many goroutines.
type Offsets struct {
	gate     Gate
	perHour  int
	distance uint32
	now      func() time.Time
	rand     func(n int) int
	log      *slog.Logger

	mu sync.Mutex
	// periods holds the occupied offsets of each kept period, by the
	// start of the period since midnight UTC.
	periods map[time.Duration][]uint32
	dialHz  uint64
	own     uint32
	hasOwn  bool
	// moves holds the times of the accepted moves of the last hour.
	moves   []time.Time
	retryAt time.Time
}

// NewOffsets returns an Offsets.
func NewOffsets(o OffsetOptions) *Offsets {
	f := &Offsets{
		gate:     o.Gate,
		perHour:  max(0, min(o.Limits.OffsetMovesPerHour, config.OffsetMovesPerHourCeiling)),
		distance: uint32(max(o.Limits.OffsetMinDistanceHz, config.OffsetDistanceFloorHz)),
		now:      o.Now, rand: o.Rand, log: o.Logger,
		periods: map[time.Duration][]uint32{},
	}
	if f.now == nil {
		f.now = time.Now
	}
	if f.rand == nil {
		f.rand = rand.IntN
	}
	if f.log == nil {
		f.log = slog.Default()
	}
	return f
}

// SetStatus records the own TX offset. A new dial forgets the occupied
// offsets of the old dial.
func (f *Offsets) SetStatus(st wsjtx.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st.DialHz != f.dialHz {
		f.periods = map[time.Duration][]uint32{}
	}
	f.dialHz, f.own, f.hasOwn = st.DialHz, st.TxDF, true
}

// Observe marks the offset of one decode as occupied.
func (f *Offsets) Observe(d wsjtx.Decode) {
	if !d.New || d.OffAir || d.Time < 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mark(d.Time, d.DeltaFreq)
}

// Spectrum marks the bins of a ws-hunter spectrum that hold a signal as
// occupied. A signal also shows a station that gave no decode.
func (f *Offsets) Spectrum(s wsjtx.SpectrumSummary) {
	if len(s.Levels) == 0 {
		return
	}
	levels := slices.Clone(s.Levels)
	slices.Sort(levels)
	noise := s.FloorDB + float64(levels[int(float64(len(levels)-1)*noisePercentile)])*s.StepDB
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mark(s.PeriodStart, 0)
	for i := range s.Levels {
		if hz, db := s.Bin(i); db >= noise+signalMarginDB {
			f.mark(s.PeriodStart, hz)
		}
	}
}

// mark adds hz to the occupied offsets of period and keeps only the
// newest periods. hz 0 only creates the period. The caller holds mu.
func (f *Offsets) mark(period time.Duration, hz uint32) {
	if _, ok := f.periods[period]; !ok {
		f.periods[period] = nil
		for len(f.periods) > keepPeriods {
			delete(f.periods, slices.Min(slices.Collect(maps.Keys(f.periods))))
		}
	}
	if _, ok := f.periods[period]; ok && hz > 0 {
		f.periods[period] = append(f.periods[period], hz)
	}
}

// Choose gives a random clear offset from 200 to 4000 Hz. A clear offset
// lies at least the offset distance from every occupied offset and from
// each offset in avoid, for example the offset of the called station. It
// returns false when no offset is clear.
func (f *Offsets) Choose(avoid ...uint32) (uint32, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.choose(avoid)
}

// choose is Choose. The caller holds mu.
func (f *Offsets) choose(avoid []uint32) (uint32, bool) {
	busy := slices.Clone(avoid)
	for _, offs := range f.periods {
		busy = append(busy, offs...)
	}
	var clear []uint32
	for hz := uint32(tx.MinTxDF); hz <= tx.MaxTxDF; hz++ {
		if f.free(hz, busy) {
			clear = append(clear, hz)
		}
	}
	if len(clear) == 0 {
		return 0, false
	}
	return clear[f.rand(len(clear))], true
}

// free reports whether hz lies at least the offset distance from each
// offset in busy.
func (f *Offsets) free(hz uint32, busy []uint32) bool {
	for _, b := range busy {
		d := hz - b
		if b > hz {
			d = b - hz
		}
		if d < f.distance {
			return false
		}
	}
	return true
}

// Hop moves the own TX offset when a move is due. The task choice calls
// it only between tasks. A move is due when a station occupies the own
// offset, before the first move, and after the interval that the moves
// per hour give. Hop sends nothing during a QSO, while auto7 does not
// run, with stock WSJT-X, and when the moves of the last hour reach the
// limit. It reports whether the gate accepted a move.
func (f *Offsets) Hop(ctx context.Context, st tx.State) bool {
	if st.Reduced() || st.Arm != tx.Running || st.QSO.Open || f.perHour <= 0 {
		return false
	}
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moves = slices.DeleteFunc(f.moves, func(t time.Time) bool { return now.Sub(t) >= time.Hour })
	if len(f.moves) >= f.perHour || now.Before(f.retryAt) {
		return false
	}
	clash := false
	var avoid []uint32
	if f.hasOwn {
		avoid = append(avoid, f.own)
		var busy []uint32
		for _, offs := range f.periods {
			busy = append(busy, offs...)
		}
		clash = !f.free(f.own, busy)
	}
	interval := time.Hour / time.Duration(f.perHour)
	if n := len(f.moves); !clash && n > 0 && now.Sub(f.moves[n-1]) < interval {
		return false
	}
	df, ok := f.choose(avoid)
	if !ok {
		return false
	}
	if d := f.gate.Submit(ctx, tx.OffsetMove{TxDF: df}); !d.OK {
		f.retryAt = now.Add(retryAfter)
		return false
	}
	f.moves = append(f.moves, now)
	f.log.Info("bands: TX offset move", "tx_df", df, "clash", clash)
	return true
}
