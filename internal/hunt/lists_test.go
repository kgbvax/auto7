// SPDX-License-Identifier: AGPL-3.0-or-later

package hunt

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClock is a settable clock for the tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

var listNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func TestStateDirUsesXDGStateHome(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	got, err := StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "auto7"); got != want {
		t.Fatalf("StateDir() = %q, want %q", got, want)
	}
}

func TestStateDirFallsBackToLocalState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)
	got, err := StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "state", "auto7"); got != want {
		t.Fatalf("StateDir() = %q, want %q", got, want)
	}
}

func TestWorkedListFileLivesInStateDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	dir, err := StateDir()
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: listNow}
	l, err := OpenLists(dir, 10*time.Minute, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.AddWorked("XX1AA", "15m", "FT8", listNow); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "auto7", "lists.json")
	if l.Path() != want {
		t.Fatalf("Path() = %q, want %q", l.Path(), want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("file not written: %v", err)
	}
}

func TestMissingFileIsEmpty(t *testing.T) {
	clk := &fakeClock{t: listNow}
	l, err := OpenLists(filepath.Join(t.TempDir(), "none"), 10*time.Minute, clk.now)
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if l.Unreadable() || l.Worked("XX1AA", "15m") || l.Skipped("XX1AA") {
		t.Fatal("missing file must give empty, readable lists")
	}
}

func TestWorkedSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: listNow}
	l, _ := OpenLists(dir, 10*time.Minute, clk.now)
	if err := l.AddWorked("xx1aa", "15m", "FT8", listNow); err != nil {
		t.Fatal(err)
	}
	r, err := OpenLists(dir, 10*time.Minute, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Worked("XX1AA", "15m") {
		t.Fatal("worked entry lost after restart")
	}
	if r.Worked("XX1AA", "20m") {
		t.Fatal("worked on 15m must not count on 20m")
	}
}

func TestWorkedExpiresAfter30Days(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: listNow}
	l, _ := OpenLists(dir, 10*time.Minute, clk.now)
	_ = l.AddWorked("XX1AA", "15m", "FT8", listNow)
	clk.t = listNow.AddDate(0, 0, 29)
	if !l.Worked("XX1AA", "15m") {
		t.Fatal("entry must hold for 29 days")
	}
	clk.t = listNow.AddDate(0, 0, 31)
	if l.Worked("XX1AA", "15m") {
		t.Fatal("entry must expire after 30 days")
	}
	r, _ := OpenLists(dir, 10*time.Minute, clk.now)
	if r.Worked("XX1AA", "15m") {
		t.Fatal("expired entry must not return after a restart")
	}
}

func TestSkipExpires(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: listNow}
	l, _ := OpenLists(dir, 10*time.Minute, clk.now)
	if err := l.Skip("XX2BB"); err != nil {
		t.Fatal(err)
	}
	clk.t = listNow.Add(9 * time.Minute)
	r, _ := OpenLists(dir, 10*time.Minute, clk.now)
	if !r.Skipped("XX2BB") {
		t.Fatal("skip must hold for the skip time, also after a restart")
	}
	clk.t = listNow.Add(10 * time.Minute)
	if r.Skipped("XX2BB") {
		t.Fatal("skip must end after the skip time")
	}
}

func TestCorruptFileIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lists.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: listNow}
	l, err := OpenLists(dir, 10*time.Minute, clk.now)
	if !errors.Is(err, ErrListsUnreadable) || !strings.Contains(err.Error(), "lists unreadable") {
		t.Fatalf("err = %v, want lists unreadable", err)
	}
	if l == nil || !l.Unreadable() || !errors.Is(l.Err(), ErrListsUnreadable) {
		t.Fatal("lists must report the unreadable state")
	}
	if err := l.AddWorked("XX1AA", "15m", "FT8", listNow); !errors.Is(err, ErrListsUnreadable) {
		t.Fatalf("write on unreadable lists: err = %v", err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "{not json" {
		t.Fatal("an unreadable file must stay as it is")
	}
}

func TestUnreadableFileIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	// A directory in place of the file cannot be read as a file.
	if err := os.Mkdir(filepath.Join(dir, "lists.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: listNow}
	if _, err := OpenLists(dir, 10*time.Minute, clk.now); !errors.Is(err, ErrListsUnreadable) {
		t.Fatalf("err = %v, want lists unreadable", err)
	}
}

func TestWriteLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: listNow}
	l, _ := OpenLists(dir, 10*time.Minute, clk.now)
	_ = l.AddWorked("XX1AA", "15m", "FT8", listNow)
	_ = l.Skip("XX2BB")
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != "lists.json" {
		t.Fatalf("directory holds %v, want only lists.json", ents)
	}
}
