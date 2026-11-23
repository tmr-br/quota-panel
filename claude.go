package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

type claudeEvent struct {
	at     time.Time
	key    string
	tokens int64
}
type tokenUsage struct {
	Input         *int64 `json:"input_tokens"`
	Output        *int64 `json:"output_tokens"`
	Creation      int64  `json:"cache_creation_input_tokens"`
	Read          int64  `json:"cache_read_input_tokens"`
	CacheCreation *struct {
		FiveMinutes int64 `json:"ephemeral_5m_input_tokens"`
		OneHour     int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func parseClaude(line []byte) (claudeEvent, bool, error) {
	var event struct {
		Type, Timestamp, UUID, SessionID, RequestID string
		Message                                     json.RawMessage
	}
	var zero claudeEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return zero, false, err
	}
	if event.Type != "assistant" {
		return zero, false, nil
	}
	var message struct {
		ID, Model string
		Usage     *tokenUsage
	}
	if err := json.Unmarshal(event.Message, &message); err != nil {
		return zero, false, err
	}
	u := message.Usage
	if u == nil || message.Model == "<synthetic>" {
		return zero, false, nil
	}
	if u.Input == nil || u.Output == nil {
		return zero, false, errors.New("incomplete token usage")
	}
	creation := u.Creation
	if split := u.CacheCreation; split != nil && (split.FiveMinutes != 0 || split.OneHour != 0) {
		if split.FiveMinutes < 0 || split.OneHour < 0 || split.FiveMinutes > math.MaxInt64-split.OneHour {
			return zero, false, errors.New("invalid cache creation token count")
		}
		// Some multi-iteration messages give a split for the first iteration
		// alongside a larger total covering all iterations.
		creation = max(creation, split.FiveMinutes+split.OneHour)
	}
	var total int64
	for _, n := range []int64{*u.Input, *u.Output, creation, u.Read} {
		if n < 0 || n > math.MaxInt64-total {
			return zero, false, errors.New("invalid token count")
		}
		total += n
	}
	at, err := time.Parse(time.RFC3339Nano, event.Timestamp)
	if err != nil {
		return zero, false, errors.New("invalid usage timestamp")
	}
	key := event.UUID
	if message.ID != "" {
		if event.RequestID != "" {
			key = message.ID + "\x00" + event.RequestID
		} else {
			key = message.ID + "\x00" + event.SessionID + "\x00" + event.Timestamp
		}
	}
	return claudeEvent{at, key, total}, true, nil
}

func fetchClaude(ctx context.Context, c claudeConfig, cache logCache[claudeEvent], now time.Time) (readings, error) {
	dir, err := logPath(c.ProjectsDir, "CLAUDE_CONFIG_DIR", ".claude", "projects")
	if err != nil {
		return nil, err
	}
	events, err := cache.read(ctx, dir, parseClaude)
	if err != nil {
		return nil, err
	}
	return claudeUsage(events, c, now)
}

func claudeUsage(events []claudeEvent, c claudeConfig, now time.Time) (readings, error) {
	// Replayed assistant turns (including subagent transcripts) can contain
	// progressively fuller snapshots of one request. Count that request once.
	byID := map[string]int{}
	unique := make([]claudeEvent, 0, len(events))
	for _, e := range events {
		if e.at.After(now) {
			continue
		}
		if e.key != "" {
			if index, ok := byID[e.key]; ok {
				prev := &unique[index]
				if e.tokens > prev.tokens {
					prev.tokens = e.tokens
				}
				if e.at.Before(prev.at) {
					prev.at = e.at
				}
				continue
			}
			byID[e.key] = len(unique)
		}
		unique = append(unique, e)
	}
	if len(unique) == 0 {
		return nil, errors.New("no Claude assistant usage records")
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i].at.Before(unique[j].at) })
	var weekEnd, blockEnd time.Time
	const week = 7 * 24 * time.Hour
	if c.WeeklyResetAnchor != "" {
		anchor, err := time.Parse(time.RFC3339, c.WeeklyResetAnchor)
		if err != nil {
			return nil, err
		}
		// Use Unix seconds: an anchor may precede or follow the current week.
		period := int64(week / time.Second)
		elapsed := ((now.Unix()-anchor.Unix())%period + period) % period
		weekEnd = time.Unix(now.Unix()-elapsed+period, 0)
	}
	var weekly, block int64
	for _, e := range unique {
		// ccusage starts blocks on a UTC clock hour. Reconstruct the sequence
		// before filtering so a rolling lookback cannot shift today's anchor.
		if blockEnd.IsZero() || e.at.After(blockEnd) {
			blockEnd = e.at.UTC().Truncate(time.Hour).Add(5 * time.Hour)
			block = 0
		}
		if e.tokens > math.MaxInt64-block {
			return nil, errors.New("block token total overflow")
		}
		block += e.tokens
		if !weekEnd.IsZero() && !e.at.Before(weekEnd.Add(-week)) {
			if e.tokens > math.MaxInt64-weekly {
				return nil, errors.New("weekly token total overflow")
			}
			weekly += e.tokens
		}
	}
	if !now.Before(blockEnd) {
		block = 0
		blockEnd = time.Time{}
	}
	out := readings{}
	if c.FiveHourTokens > 0 {
		out["5h"] = usedTokens(block, c.FiveHourTokens, blockEnd)
	}
	if c.WeeklyTokens > 0 && !weekEnd.IsZero() {
		out["week"] = usedTokens(weekly, c.WeeklyTokens, weekEnd)
	}
	for _, key := range c.Items {
		if !out[key].Valid {
			return out, fmt.Errorf("%s: set the token budget and, for weekly usage, weekly_reset_anchor", key)
		}
	}
	return out, nil
}

func usedTokens(n, limit int64, reset time.Time) reading {
	return reading{Valid: true, Percent: 100 * float64(n) / float64(limit), Reset: reset}
}
