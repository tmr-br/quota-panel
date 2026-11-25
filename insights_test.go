package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func sampleQuota(at time.Time) quotaSnapshot {
	return statusAt([]panel{
		{ID: "claude", Title: "Claude", Keys: []string{"week"}, Values: readings{"week": {Valid: true, Percent: 88, Estimated: true, Reset: at.Add(time.Hour)}}},
		{ID: "opencode", Title: "OpenCode", Keys: []string{"@shared", "quoted, model", "unknown"}, Values: readings{
			"@shared":       {Valid: true, Percent: 0, ValidUntil: at.Add(time.Minute)},
			"quoted, model": {Valid: true, Percent: -5, Estimated: true},
		}},
	}, at, at, false)
}

func TestInsightsMissingDemoAndThresholdTransitions(t *testing.T) {
	s := insights{}
	threshold := 15.0
	c := config{Insights: insightsConfig{RetentionDays: 1, AlertRemaining: &threshold}}
	sample := sampleQuota(time.Now())
	observe := func(expected int) []string {
		t.Helper()
		notes, err := s.observe(c, sample)
		if err != nil || len(notes) != expected {
			t.Fatalf("got %q, %v; expected %d alerts", notes, err, expected)
		}
		return notes
	}
	events := observe(3)
	if !strings.Contains(events[0], "est.") || !strings.Contains(events[0], "12%") || !strings.Contains(events[1], "0%") || !strings.Contains(events[2], "-5%") {
		t.Fatal(events)
	}
	observe(0)
	remaining := sample.Groups[1].Rows[0].Percent
	sample.Groups[1].Rows[0].Percent = nil
	observe(0)
	sample.Groups[1].Rows[0].Percent = remaining
	observe(0)
	sample.Demo = true
	*remaining = 30
	c.Insights.HistoryDir = filepath.Join(t.TempDir(), "uncreated")
	observe(0)
	if err := appendHistory(c.Insights, sample); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.Insights.HistoryDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	sample.Demo = false
	c.Insights.HistoryDir = ""
	if events = observe(1); !strings.Contains(events[0], "Quota recovered") {
		t.Fatal(events)
	}
	observe(0)
	c.Insights.AlertRemaining = nil
	observe(0)
	if s.low != nil {
		t.Fatal(s.low)
	}
}

func TestStatusAndHistoryDistinguishEstimatesExpiryAndZero(t *testing.T) {
	at := time.Date(2026, 8, 9, 8, 7, 6, 123, time.FixedZone("JST", 9*3600))
	s := sampleQuota(at)
	cfg := insightsConfig{HistoryDir: t.TempDir(), RetentionDays: 2}
	if err := appendHistory(cfg, s); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"json", "jsonl"} {
		var b bytes.Buffer
		if err := exportHistory(&b, cfg, format, at.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		got := quotaSnapshot{}
		if format == "jsonl" {
			if err := json.Unmarshal(b.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
		} else {
			var all []quotaSnapshot
			if err := json.Unmarshal(b.Bytes(), &all); err != nil || len(all) != 1 {
				t.Fatal(all, err)
			}
			got = all[0]
		}
		rows := got.Groups[1].Rows
		if !got.ObservedAt.Equal(at) || got.ObservedAt.Location() != time.UTC || rows[0].Percent == nil || *rows[0].Percent != 0 || rows[0].ValidUntil == nil || !rows[0].ValidUntil.Equal(at.Add(time.Minute)) || rows[0].Reset != nil || !rows[1].Estimated || rows[2].Percent != nil {
			t.Fatalf("%+v", got)
		}
	}
	var b bytes.Buffer
	if err := exportHistory(&b, cfg, "csv", at); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&b).ReadAll()
	if err != nil || len(rows) != 5 {
		t.Fatal(rows, err)
	}
	if rows[1][3] != "used" || rows[1][5] != "true" || rows[1][6] == "" || rows[2][2] != "'@shared" || rows[2][4] != "0" || rows[2][5] != "false" || rows[2][6] != "" || rows[3][2] != "quoted, model" || rows[3][4] != "-5" || rows[3][5] != "true" || rows[4][4] != "" {
		t.Fatal(rows)
	}
	b.Reset()
	if err := writeStatus(&b, s, "prometheus"); err != nil {
		t.Fatal(err)
	}
	prom := b.String()
	if !strings.Contains(prom, `quota_panel_percent{source="opencode",key="@shared",direction="remaining"} 0`) || !strings.Contains(prom, `quota_panel_available{source="opencode",key="unknown",direction="remaining"} 0`) || strings.Contains(prom, `quota_panel_percent{source="opencode",key="unknown"`) {
		t.Fatal(prom)
	}
	b.Reset()
	if err := writeStatus(&b, s, "text"); err != nil || !strings.Contains(b.String(), "-5% est.") || !strings.Contains(b.String(), "N/A") {
		t.Fatal(b.String(), err)
	}
	p := panel{ID: "claude", Keys: []string{"5h"}, Values: readings{"5h": {Valid: true, Percent: 0, ValidUntil: at.Add(time.Minute)}}}
	expired := statusAt([]panel{p}, at, at.Add(time.Minute), false).Groups[0].Rows[0]
	if expired.Percent != nil || expired.Reset != nil || expired.Estimated || expired.ValidUntil != nil {
		t.Fatal(expired)
	}
}

type historyWriteHook struct {
	bytes.Buffer
	hook func() error
}

func (h *historyWriteHook) Write(p []byte) (int, error) {
	if h.hook != nil {
		fn := h.hook
		h.hook = nil
		if err := fn(); err != nil {
			return 0, err
		}
	}
	return h.Buffer.Write(p)
}
func (h *historyWriteHook) WriteString(s string) (int, error) { return h.Write([]byte(s)) }

func TestHistoryRepairsTailAndExportsConsistentSnapshots(t *testing.T) {
	at := time.Date(2026, 8, 1, 4, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ name, tail string }{
		{"torn JSON", `{"version":1`},
		{"malformed record with newline", "{broken}\n"},
		{"several damaged writes", "{broken}\n{torn"},
		{"valid JSON without newline", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := insightsConfig{HistoryDir: t.TempDir(), RetentionDays: 2}
			s := sampleQuota(at)
			if err := appendHistory(cfg, s); err != nil {
				t.Fatal(err)
			}
			dir, _ := historyDir(cfg)
			path := filepath.Join(dir, "2026-08-01.1.jsonl")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.tail == "" {
				data = bytes.TrimSuffix(data, []byte{'\n'})
			} else {
				data = append(data, tc.tail...)
			}
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			s.ObservedAt = at.Add(time.Minute)
			h := historyWriteHook{hook: func() error { return appendHistory(cfg, s) }}
			if err = exportHistory(&h, cfg, "json", s.ObservedAt); err != nil {
				t.Fatal(err)
			}
			var before []quotaSnapshot
			if err = json.Unmarshal(h.Bytes(), &before); err != nil || len(before) != 1 || !before[0].ObservedAt.Equal(at) {
				t.Fatal(before, err)
			}
			var b bytes.Buffer
			if err = exportHistory(&b, cfg, "json", s.ObservedAt); err != nil {
				t.Fatal(err)
			}
			var after []quotaSnapshot
			if err = json.Unmarshal(b.Bytes(), &after); err != nil || len(after) != 2 || !after[1].ObservedAt.Equal(s.ObservedAt) {
				t.Fatal(after, err)
			}
		})
	}
}

func TestHistorySizeRotationAndUTCRetention(t *testing.T) {
	c := insightsConfig{HistoryDir: t.TempDir(), RetentionDays: 2}
	at := time.Date(2026, 10, 18, 23, 55, 0, 0, time.UTC)
	dir, _ := historyDir(c)
	s := sampleQuota(at)
	s.Groups[0].Rows[0].Name = strings.Repeat("a", historySegment/2)
	for i := 0; i < 3; i++ {
		s.ObservedAt = at.Add(time.Duration(i) * time.Minute)
		if err := appendHistory(c, s); err != nil {
			t.Fatal(err)
		}
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(files) != 2 {
		t.Fatal(files, err)
	}
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil || st.Size() > historySegment {
			t.Fatal(f, err)
		}
	}
	var b bytes.Buffer
	if err := exportHistory(&b, c, "json", at.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var snaps []quotaSnapshot
	if err := json.Unmarshal(b.Bytes(), &snaps); err != nil || len(snaps) != 2 || !snaps[0].ObservedAt.Equal(at.Add(time.Minute)) {
		t.Fatal(len(snaps), err)
	}
	s = sampleQuota(at.Add(6 * time.Minute).In(time.FixedZone("West", -7*3600)))
	if err := appendHistory(c, s); err != nil {
		t.Fatal(err)
	}
	s.ObservedAt = s.ObservedAt.AddDate(0, 0, 1)
	if err := appendHistory(c, s); err != nil {
		t.Fatal(err)
	}
	files, err = filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(files) != 2 || filepath.Base(files[0]) != "2026-10-19.1.jsonl" || filepath.Base(files[1]) != "2026-10-20.1.jsonl" {
		t.Fatal(files, err)
	}
	b.Reset()
	if err := exportHistory(&b, c, "json", s.ObservedAt.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b.Bytes(), &snaps); err != nil || len(snaps) != 1 || snaps[0].ObservedAt.UTC().Day() != 19 {
		t.Fatal(len(snaps), err)
	}
	s.Groups[0].Rows[0].Name = strings.Repeat("b", historySegment+1)
	if err := appendHistory(c, s); err == nil {
		t.Fatal("oversized snapshot accepted")
	}
}

func TestHistoryReportsInternalCorruption(t *testing.T) {
	c := insightsConfig{HistoryDir: t.TempDir(), RetentionDays: 2}
	at := time.Now().UTC()
	if err := appendHistory(c, sampleQuota(at)); err != nil {
		t.Fatal(err)
	}
	dir, _ := historyDir(c)
	path := filepath.Join(dir, at.Format("2006-01-02")+".1.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append([]byte("invalid interior row\n"), data...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := exportHistory(io.Discard, c, "jsonl", at); err == nil {
		t.Fatal("interior corruption silently accepted")
	}
}

func TestConcurrentHistoryAppendsPreserveRecords(t *testing.T) {
	c := insightsConfig{HistoryDir: t.TempDir(), RetentionDays: 1}
	at := time.Date(2026, 2, 10, 15, 0, 0, 0, time.UTC)
	const workers = 12
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := appendHistory(c, sampleQuota(at.Add(time.Duration(i)*time.Second))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var b bytes.Buffer
	if err := exportHistory(&b, c, "json", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var rows []quotaSnapshot
	if err := json.Unmarshal(b.Bytes(), &rows); err != nil || len(rows) != workers {
		t.Fatal(len(rows), err)
	}
	seen := map[int64]bool{}
	for _, s := range rows {
		seen[s.ObservedAt.Unix()] = true
	}
	if len(seen) != workers {
		t.Fatal(seen)
	}
}
