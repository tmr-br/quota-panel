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

func parseCodex(line []byte) (codexEvent, bool, error) {
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
	events, err := cache.read(ctx, dir, parseCodex)
	if err != nil {
		return nil, err
	}
	var latest codexEvent
	for _, e := range events {
		if !e.at.After(now) && !e.at.Before(latest.at) {
			latest = e
		}
	}
	if latest.at.IsZero() {
		return nil, errors.New("no Codex rate_limits snapshot")
	}
	out := readings{}
	var problems []error
	for _, k := range c.Items {
		w := latest.limits.Primary
		if k == "secondary" {
			w = latest.limits.Secondary
		}
		if w == nil || w.Used == nil || *w.Used < 0 || math.IsInf(*w.Used, 0) || w.Reset == nil || *w.Reset <= now.Unix() {
			problems = append(problems, fmt.Errorf("%s: missing or expired rate limit", k))
			continue
		}
		r := reading{Valid: true, Percent: *w.Used, Reset: time.Unix(*w.Reset, 0)}
		switch {
		case w.Minutes == 10080:
			r.Label = "週間枠"
		case w.Minutes > 0 && w.Minutes%60 == 0:
			r.Label = fmt.Sprintf("%d時間枠", w.Minutes/60)
		case w.Minutes > 0:
			r.Label = fmt.Sprintf("%d分枠", w.Minutes)
		}
		out[k] = r
	}
	return out, errors.Join(problems...)
}
