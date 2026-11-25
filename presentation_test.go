package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRemainingPresentationPreservesHistory(t *testing.T) {
	now := time.Date(2026, 11, 25, 16, 42, 0, 0, time.UTC)
	ps := []panel{
		{ID: "claude", Title: "Claude Code", Keys: []string{"5h"}, Values: readings{"5h": {Valid: true, Estimated: true, Percent: 39}}, FetchedAt: now},
		{ID: "codex", Title: "Codex", Keys: []string{"primary"}, Values: readings{"primary": measured(4, time.Time{})}, FetchedAt: now},
		{ID: "antigravity", Title: "Antigravity", Keys: []string{"5h"}, Values: readings{"5h": measured(80, time.Time{})}, FetchedAt: now},
		{ID: "opencode", Title: "OpenCode Go", Keys: []string{"week"}, Values: readings{"week": {Valid: true, Percent: -5}}, FetchedAt: now},
	}
	snap := statusAt(ps, now, now, false)
	var b bytes.Buffer
	if err := writeStatus(&b, snap, "text", time.UTC); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Claude Code — as of 16:42", "61% est.", "96%", "80%", "-5%"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in %s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "(left)") {
		t.Fatal("unexpected left label")
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"direction":"used"`) || !strings.Contains(string(raw), `"percent":39`) || strings.Contains(string(raw), "FetchedAt") {
		t.Fatal(string(raw))
	}
}

func TestSourceAcquisitionTime(t *testing.T) {
	now := time.Date(2026, 11, 25, 16, 42, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	p := panel{ID: "codex", Keys: []string{"primary"}, Values: readings{"primary": {Valid: true, ObservedAt: old}}}
	p.FetchedAt = acquisitionTime(p, now)
	if !p.FetchedAt.Equal(old) {
		t.Fatal(p.FetchedAt)
	}
	last := map[string]time.Time{}
	preserveAcquisitionTimes([]panel{p}, last)
	failed := []panel{{ID: "codex", FetchFailed: true}}
	preserveAcquisitionTimes(failed, last)
	if got := sourceStamp(failed[0].FetchedAt, true, now); got != "as of 15:42 (error)" {
		t.Fatal(got)
	}
	if got := sourceStamp(time.Time{}, true, now); got != "not acquired (error)" {
		t.Fatal(got)
	}
}
