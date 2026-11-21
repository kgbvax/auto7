// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/kgbvax/auto7/internal/hunt"
	"github.com/kgbvax/auto7/internal/tx"
	"github.com/kgbvax/auto7/internal/wsjtx"
)

// at gives a time on a fixed day, sec seconds after 12:00:00 UTC.
func at(sec float64) time.Time {
	return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC).Add(time.Duration(sec * float64(time.Second)))
}

func TestChoiceRunsOnceHalfASecondAfterTheLastDecode(t *testing.T) {
	s := slots{period: periodFT8}
	slot := at(0)
	if !s.decode(slot, at(13.0)) || !s.decode(slot, at(13.4)) {
		t.Fatal("decodes of a new period refused")
	}
	if _, ok := s.due(at(13.8)); ok {
		t.Fatal("due before 0.5 s after the last decode")
	}
	if got := s.next(at(13.8)); !got.Equal(at(13.9)) {
		t.Errorf("next = %s, want 13.9 s", got)
	}
	got, ok := s.due(at(13.9))
	if !ok || !got.Equal(slot) {
		t.Fatalf("due = %s %v, want the period at 0 s", got, ok)
	}
	s.ran(got)
	// A late decode of the same period gets no second run.
	if s.decode(slot, at(14.5)) {
		t.Error("late decode of a period after its run taken")
	}
	if _, ok := s.due(at(16.5)); ok {
		t.Error("second run for the same period")
	}
	// The next period runs again.
	if !s.decode(at(15), at(28)) {
		t.Fatal("decode of the next period refused")
	}
	if got, ok := s.due(at(28.5)); !ok || !got.Equal(at(15)) {
		t.Errorf("due = %s %v, want the period at 15 s", got, ok)
	}
}

func TestChoiceRunsForAPeriodWithoutDecodes(t *testing.T) {
	s := slots{period: periodFT8, done: at(0)}
	// The period at 15 s gives no decode. Its run comes 1 s into the
	// next slot.
	if _, ok := s.due(at(30.5)); ok {
		t.Fatal("silent run before 1 s into the next slot")
	}
	if got := s.next(at(30.5)); !got.Equal(at(31)) {
		t.Errorf("next = %s, want 31 s", got)
	}
	got, ok := s.due(at(31))
	if !ok || !got.Equal(at(15)) {
		t.Fatalf("due = %s %v, want the period at 15 s", got, ok)
	}
	s.ran(got)
	if _, ok := s.due(at(32)); ok {
		t.Error("second silent run")
	}
	if got := s.next(at(32)); !got.Equal(at(46)) {
		t.Errorf("next = %s, want 46 s", got)
	}
}

func TestPeriodFollowsTheMode(t *testing.T) {
	for _, c := range []struct {
		st   wsjtx.Status
		want time.Duration
	}{
		{wsjtx.Status{Mode: "FT8", TRPeriod: wsjtx.NotApplicable}, 15 * time.Second},
		{wsjtx.Status{Mode: "FT4", TRPeriod: wsjtx.NotApplicable}, 7500 * time.Millisecond},
		{wsjtx.Status{Mode: "Q65", TRPeriod: 30}, 30 * time.Second},
		{wsjtx.Status{Mode: "JT65", TRPeriod: wsjtx.NotApplicable}, 15 * time.Second},
	} {
		if got := periodOf(c.st); got != c.want {
			t.Errorf("%s: period %s, want %s", c.st.Mode, got, c.want)
		}
	}
}

func TestTaskText(t *testing.T) {
	for _, c := range []struct {
		task hunt.Task
		want string
	}{
		{hunt.Task{Kind: hunt.TaskNone, Note: hunt.NoteStopped}, "none: auto7 is stopped"},
		{hunt.Task{Kind: hunt.TaskHunt, Call: "XX1AA", Tries: 2, Requested: true, Decision: tx.Decision{OK: true}}, "hunt XX1AA, try 2"},
		{hunt.Task{Kind: hunt.TaskAnswer, Call: "XY3CC", Requested: true, Decision: tx.Decision{Reason: "too late for this slot"}}, "answer XY3CC (refused: too late for this slot)"},
	} {
		if got := taskText(c.task); got != c.want {
			t.Errorf("taskText = %q, want %q", got, c.want)
		}
	}
}

func TestUsageNamesThePasswordVariable(t *testing.T) {
	var b bytes.Buffer
	if code := run([]string{"-h"}, &b); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	for _, want := range []string{"-config", "Environment:", "AUTO7_MQTT_PASSWORD"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("usage lacks %q:\n%s", want, b.String())
		}
	}
}

func TestMissingConfigFileGivenByFlagIsAnError(t *testing.T) {
	var b bytes.Buffer
	if code := run([]string{"-config", t.TempDir() + "/none.toml"}, &b); code != 1 {
		t.Errorf("exit code %d, want 1:\n%s", code, b.String())
	}
}
