package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A failed or partial fetch keeps the last good rows and their acquisition
// time; a column that was never read stays unavailable.
func TestRevalidateKeepsLastGoodReadings(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 17, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Minute)
	fresh := func(values readings, failed bool) panel {
		return panel{ID: "opencode", Keys: []string{"5h", "week"}, Values: values, FetchFailed: failed}
	}
	first := revalidate(panel{}, false, fresh(readings{"5h": measured(90, time.Time{})}, true), t0)
	if !first.Values["5h"].Valid || first.Values["week"].Valid || !first.FetchedAt.Equal(t0) {
		t.Fatalf("first: %+v", first)
	}

	// Whole fetch failed: every row keeps its value; as of stays at t0.
	failed := revalidate(first, true, fresh(nil, true), t1)
	if v := failed.Values["5h"]; !v.Valid || v.Percent != 90 || !failed.FetchedAt.Equal(t0) || !failed.FetchFailed {
		t.Fatalf("failed: %+v", failed)
	}
	if got := sourceStamp(failed.FetchedAt, failed.FetchFailed, t1); got != "as of 17:00 (error)" {
		t.Fatal(got)
	}
	// Never-read row is still unavailable rather than 0%.
	if failed.Values["week"].Valid {
		t.Fatal("never-read row became valid")
	}

	// Partial: the new row replaces, the missing row keeps t0, so the
	// column's as of reports the older of the two.
	partial := revalidate(failed, true, fresh(readings{"week": measured(40, time.Time{})}, false), t1)
	if partial.Values["5h"].Percent != 90 || partial.Values["week"].Percent != 40 || !partial.FetchedAt.Equal(t0) {
		t.Fatalf("partial: %+v", partial)
	}
	// Once both rows are fresh, as of advances.
	t2 := t1.Add(5 * time.Minute)
	full := revalidate(partial, true, fresh(readings{"5h": measured(80, time.Time{}), "week": measured(39, time.Time{})}, false), t2)
	if !full.FetchedAt.Equal(t2) || full.Values["5h"].Percent != 80 {
		t.Fatalf("full: %+v", full)
	}
}

// The renderer keeps a reading that passed its reset, dimmed, instead of N/A.
func TestExpiredReadingStaysOnPanel(t *testing.T) {
	r, err := newRenderer(catalogSize())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 25, 17, 0, 0, 0, time.UTC)
	p := panel{ID: "codex", Keys: []string{"primary"}, Values: readings{"primary": measured(40, now.Add(-time.Second))}}
	v, valueInk, _, note := r.shownAt(p, "primary", now)
	if !v.Valid || v.Percent != 40 || valueInk != muted || note != "reset passed · refreshing" {
		t.Fatalf("%+v %v %q", v, valueInk, note)
	}
	never := panel{ID: "codex", Keys: []string{"primary"}}
	if v, _, _, _ := r.shownAt(never, "primary", now); v.Valid {
		t.Fatal("never-read row must stay N/A")
	}
}

// End to end: nothing is published before the first fetch, the System column
// refreshes on its own short interval, and a failing refresh keeps showing
// the previous values with their original as-of time.
func TestSystemRefreshKeepsPreviousValuesOnFailure(t *testing.T) {
	var calls atomic.Int32
	good := sampleCommands()
	systemRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "iostat" {
			calls.Add(1)
		}
		if calls.Load() >= 2 {
			return nil, errors.New("simulated failure")
		}
		return fakeRunner(good)(ctx, name, args...)
	}
	defer func() { systemRunner = runCommand }()

	path := filepath.Join(t.TempDir(), "settings.toml")
	writeFixture(t, path, "interval_seconds=300\ndisplay_order=['system']\n[system]\nenabled=true\ninterval_seconds=1\nitems=['cpu','swap']\n")
	c, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var mu sync.Mutex
	var got []panel
	var quotaCycles int
	pollSources(ctx, path, c, func(_ config, shown, raw []panel, _ time.Time) {
		mu.Lock()
		defer mu.Unlock()
		if len(shown) != 1 || shown[0].ID != "system" {
			t.Errorf("unexpected snapshot %+v", shown)
			return
		}
		if raw != nil {
			quotaCycles++
		}
		got = append(got, shown[0])
		if len(got) == 3 {
			cancel()
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if len(got) < 3 {
		t.Fatalf("only %d snapshots", len(got))
	}
	first := got[0]
	if !first.Values["cpu"].Valid || first.FetchFailed || first.FetchedAt.IsZero() {
		t.Fatalf("first snapshot incomplete: %+v", first)
	}
	last := got[len(got)-1]
	if !last.FetchFailed {
		t.Fatal("later refresh should have failed")
	}
	for _, k := range []string{"cpu", "swap"} {
		if v := last.Values[k]; !v.Valid || v.Percent != first.Values[k].Percent {
			t.Errorf("%s not kept after failure: %+v", k, v)
		}
	}
	if !last.FetchedAt.Equal(first.FetchedAt) {
		t.Errorf("as of moved to %v, want %v", last.FetchedAt, first.FetchedAt)
	}
	if quotaCycles > 1 {
		t.Errorf("System refreshes were reported as %d quota cycles", quotaCycles)
	}
}
