package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestTrendsHistoryBoundariesDirectionsAndMissingValues(t *testing.T) {
	now := time.Date(2026, 11, 23, 0, 15, 0, 0, time.UTC)
	begin := now.Truncate(24 * time.Hour)
	cfg := insightsConfig{HistoryDir: t.TempDir(), RetentionDays: 3}
	oneHundred, twenty, zero, over := 100.0, 20.0, 0.0, 108.0
	write := func(at time.Time, dir string, row quotaRow) {
		t.Helper()
		snap := quotaSnapshot{Version: 1, ObservedAt: at, Groups: []quotaGroup{{ID: "codex", Direction: dir, Rows: []quotaRow{row}}}}
		if err := appendHistory(cfg, snap); err != nil {
			t.Fatal(err)
		}
	}
	row := func(k string, n *float64) quotaRow { return quotaRow{Key: k, Percent: n} }
	write(begin.Add(-time.Nanosecond), "used", row("before-midnight", &zero))
	write(now.Add(time.Nanosecond), "used", row("future", &zero))
	write(begin, "used", row("primary", &oneHundred))
	// Reset after the observation but before now; this historic zero is valid.
	reset := begin.Add(time.Second)
	write(begin.Add(time.Millisecond), "used", quotaRow{Key: "primary", Percent: &oneHundred, Reset: &reset})
	write(begin.Add(2*time.Minute), "used", row("primary", &over))
	write(begin.Add(3*time.Minute), "used", quotaRow{Key: "primary", Percent: &twenty, Estimated: true})
	write(now, "used", row("primary", nil))
	// Out-of-order appends must not overwrite the latest observation. A row
	// whose cache had already expired at the time is another unavailable poll.
	write(begin.Add(5*time.Minute), "used", quotaRow{Key: "primary", Percent: &twenty, ValidUntil: &reset})
	write(now, "remaining", row("primary", &twenty))
	write(now, "used", row("missing", nil))
	var out bytes.Buffer
	if err := writeTrends(context.Background(), &out, cfg, now.In(time.FixedZone("JST", 9*3600)), 1); err != nil {
		t.Fatal(err)
	}
	report := out.String()
	for _, want := range []string{"Nov 23 2026 00:00 UTC", "multiple accounts", `"codex" / "missing"  used converted to left`, "Valid: 0  N/A: 1", `"codex" / "primary"  left`, "reported 1  min left 20%  max left 20%", "Valid: 4  N/A: 2", "reported 3  min left -8%  max left 0%", "est. 1  min left 80%  max left 80%", "Last record Nov 23 2026 00:15:00 UTC  N/A"} {
		if !strings.Contains(report, want) {
			t.Errorf("missing %q in\n%s", want, report)
		}
	}
	for _, excluded := range []string{"before-midnight", "future"} {
		if strings.Contains(report, excluded) {
			t.Fatal(report)
		}
	}
}

func TestTrendsConflictingLatestRecords(t *testing.T) {
	now := time.Now().UTC()
	a := trendValue{available: true, remaining: 0}
	b := a
	b.estimated = true
	for _, values := range [][]trendValue{{a, b, a}, {b, a, b}} {
		var s trendSeries
		for _, v := range values {
			s.add(now, v)
		}
		if !s.conflict || s.reported.count+s.estimated.count != 3 {
			t.Fatal(s)
		}
		s.add(now.Add(-time.Minute), a)
		if !s.conflict {
			t.Fatal("earlier row cleared conflict")
		}
		s.add(now.Add(time.Minute), trendValue{})
		if s.conflict || s.lastValue.available {
			t.Fatal(s)
		}
	}
}

type failingTrendWriter struct{}

func (failingTrendWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestTrendsEmptyRangeErrorsAndCancellation(t *testing.T) {
	cfg := insightsConfig{HistoryDir: t.TempDir(), RetentionDays: 2}
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	for _, d := range []int{-1, 0, 91} {
		if err := writeTrends(context.Background(), io.Discard, cfg, now, d); err == nil {
			t.Fatalf("accepted %d days", d)
		}
	}
	var out bytes.Buffer
	if err := writeTrends(context.Background(), &out, cfg, now, 90); err != nil || !strings.Contains(out.String(), "Nov 22 2026 00:00 UTC") || !strings.Contains(out.String(), "No records in this period") {
		t.Fatal(out.String(), err)
	}
	if err := writeTrends(context.Background(), failingTrendWriter{}, cfg, now, 1); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writeTrends(ctx, io.Discard, cfg, now, 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	cfg.HistoryDir = ""
	if err := writeTrends(context.Background(), io.Discard, cfg, now, 1); err == nil {
		t.Fatal("disabled history must report configuration error")
	}
}

func TestTrendsSeparatesCodexWindowChanges(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	cfg := insightsConfig{HistoryDir: t.TempDir(), RetentionDays: 2}
	for i, name := range []string{"5時間枠", "週間枠", "Primary"} {
		p := float64(i*80 + 10)
		r := quotaRow{Key: "primary", Name: name, Percent: &p}
		if i == 2 {
			r.Percent = nil
		}
		s := quotaSnapshot{Version: 1, ObservedAt: now.Add(time.Duration(i-2) * time.Minute), Groups: []quotaGroup{{ID: "codex", Direction: "used", Rows: []quotaRow{r}}}}
		if err := appendHistory(cfg, s); err != nil {
			t.Fatal(err)
		}
	}
	var b bytes.Buffer
	if err := writeTrends(context.Background(), &b, cfg, now, 2); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\"5h\"\n  Valid: 1  N/A: 0\n  reported 1  min left 90%", "\"weekly\"\n  Valid: 1  N/A: 0\n  reported 1  min left 10%", "\"window unknown\"\n  Valid: 0  N/A: 1"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in %s", want, b.String())
		}
	}
}

func TestTrendsFractionalExtrema(t *testing.T) {
	for input, want := range map[float64]string{100 - 98.8: "1.2", 10.1: "10.1", 10.4: "10.4", 0.001: "0.001", 99.999999999: "<100", 100: "100"} {
		if got := trendPercent(input); got != want {
			t.Errorf("trendPercent(%v) = %s, want %s", input, got, want)
		}
	}
}
