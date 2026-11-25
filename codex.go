package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

type codexWindow struct {
	Used    *float64 `json:"used_percent"`
	Minutes int64    `json:"window_minutes"`
	Reset   *int64   `json:"resets_at"`
}
type codexLimits struct {
	ID                 string `json:"limit_id"`
	Primary, Secondary *codexWindow
}
type codexEvent struct {
	at     time.Time
	limits codexLimits
}

// Tied timestamps have no ordering across rollouts. Keep a window only when
// both observations describe the same measurement. A conflict stays unknown
// through further ties and is cleared by a strictly newer snapshot.
func commonCodexWindow(a, b *codexWindow) *codexWindow {
	if a == nil || b == nil || a.Minutes != b.Minutes {
		return nil
	}
	if a.Used == nil || b.Used == nil || *a.Used != *b.Used {
		return nil
	}
	if a.Reset == nil || b.Reset == nil || *a.Reset != *b.Reset {
		return nil
	}
	return a
}

func parseCodex(_ string, line []byte) (codexEvent, bool, error) {
	var meta struct {
		Type, Timestamp string
		Payload         json.RawMessage
	}
	var zero codexEvent
	if err := json.Unmarshal(line, &meta); err != nil {
		return zero, false, err
	}
	if meta.Type != "event_msg" {
		return zero, false, nil
	}
	var event struct {
		Type   string
		Limits *codexLimits `json:"rate_limits"`
	}
	if err := json.Unmarshal(meta.Payload, &event); err != nil {
		return zero, false, err
	}
	if event.Type != "token_count" || event.Limits == nil || (event.Limits.ID != "" && event.Limits.ID != "codex") {
		return zero, false, nil
	}
	at, err := time.Parse(time.RFC3339Nano, meta.Timestamp)
	if err != nil {
		return zero, false, errors.New("invalid rate_limits timestamp")
	}
	return codexEvent{at, *event.Limits}, true, nil
}

func fetchCodex(ctx context.Context, c codexConfig, cache logCache[codexEvent], now time.Time) (readings, error) {
	dir, err := logPath(c.SessionsDir, "CODEX_HOME", ".codex", "sessions")
	if err != nil {
		return nil, err
	}
	var problems []error
	var lastDamagedWrite time.Time
	events, err := cache.readReportingMalformed(ctx, dir, parseCodex, func(modified time.Time, problem error) {
		problems = append(problems, problem)
		if modified.After(lastDamagedWrite) {
			lastDamagedWrite = modified
		}
	})
	if err != nil {
		return nil, err
	}
	var latest codexEvent
	for _, e := range events {
		if e.at.After(now) || e.at.Before(latest.at) {
			continue
		}
		if e.at.Equal(latest.at) {
			latest.limits.Primary = commonCodexWindow(latest.limits.Primary, e.limits.Primary)
			latest.limits.Secondary = commonCodexWindow(latest.limits.Secondary, e.limits.Secondary)
			continue
		}
		latest = e
	}
	if latest.at.IsZero() {
		return nil, errors.Join(append(problems, errors.New("no Codex rate_limits snapshot"))...)
	}
	// A crashed session can leave a torn record. Only a snapshot recorded
	// strictly after the last write to every damaged log supersedes it.
	if len(problems) > 0 {
		damage := errors.Join(problems...)
		if !latest.at.After(lastDamagedWrite) {
			return nil, fmt.Errorf("cannot establish the latest Codex snapshot: %w", damage)
		}
		problems = []error{fmt.Errorf("using a snapshot newer than the damaged Codex logs: %w", damage)}
	}
	out := readings{}
	for _, k := range c.Items {
		w := latest.limits.Primary
		if k == "secondary" {
			w = latest.limits.Secondary
		}
		if w == nil || w.Used == nil || *w.Used < 0 || math.IsInf(*w.Used, 0) || w.Reset == nil || *w.Reset <= now.Unix() {
			problems = append(problems, fmt.Errorf("%s: missing, conflicting, or expired rate limit", k))
			continue
		}
		r := reading{Valid: true, Percent: *w.Used, ObservedAt: latest.at, Reset: time.Unix(*w.Reset, 0)}
		switch {
		case w.Minutes == 10080:
			r.Label = "weekly"
		case w.Minutes > 0 && w.Minutes%60 == 0:
			r.Label = fmt.Sprintf("%dh", w.Minutes/60)
		case w.Minutes > 0:
			r.Label = fmt.Sprintf("%dm", w.Minutes)
		}
		out[k] = r
	}
	return out, errors.Join(problems...)
}
