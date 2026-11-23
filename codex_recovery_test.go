package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexSupersedesOnlyOlderDamagedLogs(t *testing.T) {
	dir := t.TempDir()
	c := codexConfig{selection: selection{Items: []string{"primary", "secondary"}}, SessionsDir: dir}
	now := time.Date(2026, 4, 9, 14, 37, 0, 0, time.UTC)
	snapshot := now.Add(-5 * time.Minute)
	cache := logCache[codexEvent]{}
	active := filepath.Join(dir, "active.jsonl")
	old := filepath.Join(dir, "crashed.jsonl")
	writeFixture(t, old, `{"timestamp":`)
	event := fmt.Sprintf(`{"type":"event_msg","timestamp":%q,"payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":17,"resets_at":%d},"secondary":{"used_percent":72,"resets_at":%d}}}}`,
		snapshot.Format(time.RFC3339), now.Add(time.Hour).Unix(), now.Add(48*time.Hour).Unix())
	writeFixture(t, active, event)

	for _, tc := range []struct {
		name     string
		modified time.Time
		valid    bool
	}{
		{"older crash", snapshot.Add(-24 * time.Hour), true},
		{"same time", snapshot, false},
		{"newer crash", snapshot.Add(time.Minute), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Chtimes(old, tc.modified, tc.modified); err != nil {
				t.Fatal(err)
			}
			values, err := fetchCodex(context.Background(), c, cache, now)
			if err == nil || !strings.Contains(err.Error(), "crashed.jsonl") {
				t.Fatalf("missing damaged file diagnostic: %v", err)
			}
			if values["primary"].Valid != tc.valid || values["secondary"].Valid != tc.valid {
				t.Fatalf("readings = %v, want valid = %t", values, tc.valid)
			}
			if tc.valid && (values["primary"].Percent != 17 || values["secondary"].Percent != 72) {
				t.Fatal(values)
			}
		})
	}
	writeFixture(t, old, `{"type":"session_meta"}`)
	values, err := fetchCodex(context.Background(), c, cache, now)
	if err != nil || !values["primary"].Valid || !values["secondary"].Valid {
		t.Fatalf("repair did not clear old diagnostic: %v %v", values, err)
	}
}

func TestCodexEqualTimestampWindowsAgreePerWindow(t *testing.T) {
	at := time.Date(2026, 4, 9, 14, 37, 0, 0, time.UTC)
	now := at.Add(time.Minute)
	reset := at.Add(time.Hour).Unix()
	window := func(percent, minutes int, end int64) string {
		return fmt.Sprintf(`{"used_percent":%d,"window_minutes":%d,"resets_at":%d}`, percent, minutes, end)
	}
	event := func(ts time.Time, primary, secondary string) string {
		return fmt.Sprintf(`{"type":"event_msg","timestamp":%q,"payload":{"type":"token_count","rate_limits":{"primary":%s,"secondary":%s}}}`+"\n",
			ts.Format(time.RFC3339Nano), primary, secondary)
	}
	primary, secondary := window(12, 300, reset), window(84, 10080, reset+86400)
	for _, tc := range []struct {
		name, first, second        string
		wantPrimary, wantSecondary bool
	}{
		{"same measurements", primary, secondary, true, true},
		{"used percent differs", window(13, 300, reset), secondary, false, true},
		{"reset differs", primary, window(84, 10080, reset), true, false},
		{"window duration differs", window(12, 60, reset), secondary, false, true},
		{"one window absent", primary, "null", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, reversed := range []bool{false, true} {
				dir := t.TempDir()
				c := codexConfig{selection: selection{Items: []string{"primary", "secondary"}}, SessionsDir: dir}
				cache := logCache[codexEvent]{}
				original := event(at, primary, secondary)
				first, second := original, event(at, tc.first, tc.second)
				if reversed {
					first, second = second, first
				}
				writeFixture(t, filepath.Join(dir, "a.jsonl"), first)
				writeFixture(t, filepath.Join(dir, "b.jsonl"), second+original)
				values, err := fetchCodex(context.Background(), c, cache, now)
				if values["primary"].Valid != tc.wantPrimary || values["secondary"].Valid != tc.wantSecondary ||
					(err == nil) != (tc.wantPrimary && tc.wantSecondary) {
					t.Fatalf("reversed=%v: %v %v", reversed, values, err)
				}
				writeFixture(t, filepath.Join(dir, "new.jsonl"), event(now, window(51, 300, reset), secondary))
				values, err = fetchCodex(context.Background(), c, cache, now)
				if err != nil || !values["primary"].Valid || values["primary"].Percent != 51 || !values["secondary"].Valid {
					t.Fatalf("newer snapshot did not recover: %v %v", values, err)
				}
			}
		})
	}
}
