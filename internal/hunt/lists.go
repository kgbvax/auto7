// SPDX-License-Identifier: AGPL-3.0-or-later

package hunt

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ListsUnreadable is the reason text when the list file cannot be read.
const ListsUnreadable = "lists unreadable"

// ErrListsUnreadable marks every error of a list file that exists but
// cannot be read or parsed. The gate refuses Enable while it holds.
var ErrListsUnreadable = errors.New(ListsUnreadable)

// WorkedDays is the number of days that the worked list keeps an entry.
const WorkedDays = 30

// listsFileName is the name of the list file in the state directory.
const listsFileName = "lists.json"

// listsVersion is the format version of the list file.
const listsVersion = 1

// dateLayout is the layout of the UTC date of a worked entry.
const dateLayout = "2006-01-02"

// StateDir returns the state directory of auto7. It is
// $XDG_STATE_HOME/auto7 if that variable is set, else
// ~/.local/state/auto7. The directory is outside the repository.
func StateDir() (string, error) {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "auto7"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("state directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "auto7"), nil
}

// WorkedEntry is one station that auto7 worked.
type WorkedEntry struct {
	Call string `json:"call"`
	Band string `json:"band"`
	Mode string `json:"mode"`
	// Date is the UTC date of the QSO, for example "2026-11-21".
	Date string `json:"date"`
}

// SkipEntry is one station that auto7 skips until a time.
type SkipEntry struct {
	Call  string    `json:"call"`
	Until time.Time `json:"until"`
}

// listsFile is the JSON of the list file.
type listsFile struct {
	Version int           `json:"version"`
	Worked  []WorkedEntry `json:"worked"`
	Skip    []SkipEntry   `json:"skip"`
}

// Lists holds the worked list and the skip list in one file. All methods
// are safe for use by many goroutines.
type Lists struct {
	mu      sync.Mutex
	path    string
	skipFor time.Duration
	now     func() time.Time
	err     error
	worked  []WorkedEntry
	skip    map[string]time.Time
}

// OpenLists reads the list file in dir. A missing file gives empty
// lists. skipFor is the time that Skip puts a station on the skip list.
//
// If the file exists but cannot be read or parsed, OpenLists returns
// lists in the unreadable state together with an error that wraps
// ErrListsUnreadable. In that state the lists are empty, and every write
// fails, so the file stays as it is for the operator to inspect.
func OpenLists(dir string, skipFor time.Duration, now func() time.Time) (*Lists, error) {
	l := &Lists{
		path:    filepath.Join(dir, listsFileName),
		skipFor: skipFor,
		now:     now,
		skip:    map[string]time.Time{},
	}
	b, err := os.ReadFile(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err == nil {
		err = l.parse(b)
	}
	if err != nil {
		l.err = fmt.Errorf("%w: %s: %v", ErrListsUnreadable, l.path, err)
		l.worked, l.skip = nil, map[string]time.Time{}
		return l, l.err
	}
	l.prune()
	return l, nil
}

func (l *Lists) parse(b []byte) error {
	var f listsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	if f.Version != listsVersion {
		return fmt.Errorf("format version %d, want %d", f.Version, listsVersion)
	}
	for _, w := range f.Worked {
		if _, err := time.Parse(dateLayout, w.Date); err != nil {
			return fmt.Errorf("worked %s: date %q", w.Call, w.Date)
		}
	}
	l.worked = f.Worked
	for _, s := range f.Skip {
		l.skip[normCall(s.Call)] = s.Until
	}
	return nil
}

// Path returns the path of the list file.
func (l *Lists) Path() string { return l.path }

// Err returns the error of an unreadable list file, or nil.
func (l *Lists) Err() error { return l.err }

// Unreadable reports whether the list file exists but cannot be read.
func (l *Lists) Unreadable() bool { return l.err != nil }

// AddWorked records a QSO with call on band and mode at the time at, and
// writes the file.
func (l *Lists) AddWorked(call, band, mode string, at time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.worked = append(l.worked, WorkedEntry{
		Call: normCall(call), Band: band, Mode: mode, Date: at.UTC().Format(dateLayout),
	})
	l.prune()
	return l.save()
}

// Worked reports whether auto7 worked call on band in the last
// WorkedDays days, in any mode.
func (l *Lists) Worked(call, band string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	call = normCall(call)
	cut := l.cutoff()
	for _, w := range l.worked {
		if w.Call == call && w.Band == band && w.Date >= cut {
			return true
		}
	}
	return false
}

// Skip puts call on the skip list for the skip time, and writes the
// file.
func (l *Lists) Skip(call string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.skip[normCall(call)] = l.now().Add(l.skipFor)
	l.prune()
	return l.save()
}

// Skipped reports whether call is on the skip list now.
func (l *Lists) Skipped(call string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	until, ok := l.skip[normCall(call)]
	return ok && l.now().Before(until)
}

// cutoff returns the oldest UTC date that the worked list keeps. An
// entry is kept for WorkedDays days after its date.
func (l *Lists) cutoff() string {
	return l.now().UTC().AddDate(0, 0, -WorkedDays).Format(dateLayout)
}

// prune drops old worked entries and ended skips. The caller holds mu or
// owns l alone.
func (l *Lists) prune() {
	cut := l.cutoff()
	kept := l.worked[:0]
	for _, w := range l.worked {
		if w.Date >= cut {
			kept = append(kept, w)
		}
	}
	l.worked = kept
	now := l.now()
	for c, until := range l.skip {
		if !now.Before(until) {
			delete(l.skip, c)
		}
	}
}

// save writes the file atomically: it writes a temporary file in the
// same directory and renames it. The caller holds mu.
func (l *Lists) save() error {
	f := listsFile{Version: listsVersion, Worked: l.worked, Skip: make([]SkipEntry, 0, len(l.skip))}
	if f.Worked == nil {
		f.Worked = []WorkedEntry{}
	}
	for c, until := range l.skip {
		f.Skip = append(f.Skip, SkipEntry{Call: c, Until: until.UTC()})
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("lists: %w", err)
	}
	tmp, err := os.CreateTemp(dir, listsFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("lists: %w", err)
	}
	name := tmp.Name()
	_, err = tmp.Write(b)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, l.path)
	}
	if err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("lists: %w", err)
	}
	return nil
}

// normCall returns the call in upper case without angle brackets.
func normCall(call string) string {
	return strings.ToUpper(strings.Trim(strings.TrimSpace(call), "<>"))
}
