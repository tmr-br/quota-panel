package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// A zero-value reading is unavailable. A measured 0% must be explicit.
type reading struct {
	Percent    float64
	Valid      bool
	Estimated  bool
	Reset      time.Time
	ValidUntil time.Time // Cache expiry when independent from the quota reset.
	Label      string
	ObservedAt time.Time // Timestamp of a source-reported snapshot, if available.
	Detail     string    // Secondary line for system metrics, e.g. "6.1 of 7.0 GB swapped".
}

func (r reading) expiresAt() time.Time {
	if r.Reset.IsZero() || (!r.ValidUntil.IsZero() && r.ValidUntil.Before(r.Reset)) {
		return r.ValidUntil
	}
	return r.Reset
}

func measured(p float64, reset time.Time) reading {
	if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 100 {
		return reading{}
	}
	return reading{Percent: p, Valid: true, Reset: reset}
}

type readings map[string]reading

type panel struct {
	ID, Title   string
	Subtitle    string // Small marker under the as-of time, e.g. the host name.
	Keys        []string
	Values      readings
	FetchedAt   time.Time
	FetchFailed bool
}

func (p panel) valueAt(key string, now time.Time) reading {
	v := p.Values[key]
	if expiry := v.expiresAt(); !expiry.IsZero() && !expiry.After(now) {
		v.Valid = false
	}
	return v
}

func remainingPercent(id string, percent float64) float64 {
	if id == "claude" || id == "codex" {
		return 100 - percent
	}
	return percent
}

func sourceStamp(at time.Time, failed bool, now time.Time) string {
	if at.IsZero() {
		if failed {
			return "not acquired (error)"
		}
		return "not acquired"
	}
	at = at.In(now.Location())
	format := "15:04"
	if at.Format("2006-01-02") != now.Format("2006-01-02") {
		format = "Jan 2 15:04"
	}
	label := "as of " + at.Format(format)
	if failed {
		label += " (error)"
	}
	return label
}

func panels(c config) []panel {
	var out []panel
	for _, id := range c.Order {
		var p panel
		var on bool
		switch id {
		case "claude":
			p = panel{ID: id, Title: "Claude Code", Keys: c.Claude.Items}
			on = c.Claude.Enabled
		case "codex":
			p = panel{ID: id, Title: "Codex", Keys: c.Codex.Items}
			on = c.Codex.Enabled
		case "antigravity":
			p = panel{ID: id, Title: "Antigravity", Keys: c.Antigravity.Items}
			on = c.Antigravity.Enabled
		case "opencode":
			p = panel{ID: id, Title: "OpenCode Go", Keys: c.OpenCode.Items}
			on = c.OpenCode.Enabled
		case "system":
			p = panel{ID: id, Title: "System", Subtitle: hostName(), Keys: c.System.Items}
			on = c.System.Enabled
		}
		if on && len(p.Keys) > 0 {
			out = append(out, p)
		}
	}
	return out
}

func rowLabel(p panel, key string) string {
	if label := p.Values[key].Label; label != "" {
		return label
	}
	if p.ID == "system" {
		switch key {
		case "cpu":
			return "CPU"
		case "memory":
			return "Memory"
		case "swap":
			return "Swap"
		case "disk":
			return "Storage free"
		}
	}
	switch key {
	case "5h":
		return "5h"
	case "week":
		return "weekly"
	case "month":
		return "monthly"
	case "primary":
		return "Primary"
	case "secondary":
		return "Secondary"
	}
	if p.ID == "antigravity" {
		group, window, _ := strings.Cut(key, ".")
		label := "Gemini"
		if group == "claude_gpt" {
			label = "Claude / GPT"
		}
		if window == "5h" {
			return label + "  ·  5h"
		}
		return label + "  ·  weekly"
	}
	return key
}

func demoPanels(c config, now time.Time) []panel {
	out := panels(c)
	examples := map[string][]float64{"claude": {36, 62}, "codex": {18, 47}, "antigravity": {82, 54, 70, 38}, "opencode": {94, 76, 58, 32}}
	for i, p := range out {
		if p.ID == "system" {
			details := map[string]reading{
				"cpu":    {Percent: 23, Valid: true, Detail: "user 15% · sys 8%"},
				"memory": {Percent: 74, Valid: true, Detail: "11.8 of 16.0 GB used"},
				"swap":   {Percent: 86, Valid: true, Detail: "6.0 of 7.0 GB swapped"},
				"disk":   {Percent: 37, Valid: true, Detail: "183 GB free of 494 GB"},
			}
			sample := readings{}
			for _, key := range p.Keys {
				sample[key] = details[key]
			}
			out[i].Values, out[i].FetchedAt = sample, now
			continue
		}
		sample := readings{}
		for j, key := range p.Keys {
			reset := now.Add(time.Duration(150+j*21) * time.Minute)
			if strings.HasSuffix(key, "week") || key == "secondary" {
				reset = now.Add(51 * time.Hour)
			}
			r := measured(examples[p.ID][j%len(examples[p.ID])], reset)
			if key == "primary" {
				r.Label = "5h"
			}
			if key == "secondary" {
				r.Label = "weekly"
			}
			sample[key] = r
		}
		out[i].Values = sample
		out[i].FetchedAt = now
	}
	return out
}

func resetText(id string, r reading, now time.Time) string {
	if !r.Valid {
		return ""
	}
	if id == "system" {
		return r.Detail
	}
	if r.Reset.IsZero() {
		// An unused rolling window has no reset until its first request.
		if (id == "claude" && r.Percent == 0) || (id == "opencode" && r.Percent == 100) {
			return "starts on next use"
		}
		return "reset unknown"
	}
	if id != "claude" {
		return "resets " + r.Reset.In(now.Location()).Format("Jan 2 15:04")
	}
	mins := int(math.Ceil(r.Reset.Sub(now).Minutes()))
	if mins < 0 {
		mins = 0
	}
	if mins >= 1440 {
		return fmt.Sprintf("resets in %dd %dh", mins/1440, (mins%1440)/60)
	}
	if mins >= 60 {
		return fmt.Sprintf("resets in %dh %dm", mins/60, mins%60)
	}
	return fmt.Sprintf("resets in %dm", mins)
}
