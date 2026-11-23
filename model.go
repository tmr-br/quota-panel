package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// A zero-value reading is unavailable. A measured 0% must be explicit.
type reading struct {
	Percent float64
	Valid   bool
	Reset   time.Time
	Label   string
}

func measured(p float64, reset time.Time) reading {
	if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 100 {
		return reading{}
	}
	return reading{Percent: p, Valid: true, Reset: reset}
}

type readings map[string]reading

type panel struct {
	ID, Title, Unit string
	Keys            []string
	Values          readings
}

func (p panel) valueAt(key string, now time.Time) reading {
	v := p.Values[key]
	if !v.Reset.IsZero() && !v.Reset.After(now) {
		v.Valid = false
	}
	return v
}

func panels(c config) []panel {
	var out []panel
	for _, id := range c.Order {
		var p panel
		var on bool
		switch id {
		case "claude":
			p = panel{ID: id, Title: "Claude Code", Unit: "推定使用率", Keys: c.Claude.Items}
			on = c.Claude.Enabled
		case "codex":
			p = panel{ID: id, Title: "Codex", Unit: "使用率", Keys: c.Codex.Items}
			on = c.Codex.Enabled
		case "antigravity":
			p = panel{ID: id, Title: "Antigravity", Unit: "プール別残量", Keys: c.Antigravity.Items}
			on = c.Antigravity.Enabled
		case "opencode":
			p = panel{ID: id, Title: "OpenCode Go", Unit: "5時間枠の残量", Keys: c.OpenCode.Models}
			on = c.OpenCode.Enabled
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
	switch key {
	case "5h":
		return "5時間枠"
	case "week":
		return "週間枠"
	case "primary":
		return "Primary"
	case "secondary":
		return "Secondary"
	case "@shared":
		return "アカウント共通"
	}
	if p.ID == "antigravity" {
		group, window, _ := strings.Cut(key, ".")
		label := "Gemini"
		if group == "claude_gpt" {
			label = "Claude / GPT"
		}
		if window == "5h" {
			return label + "  ·  5時間"
		}
		return label + "  ·  週間"
	}
	return key
}

func demoPanels(c config, now time.Time) []panel {
	out := panels(c)
	examples := map[string][]float64{"claude": {36, 62}, "codex": {18, 47}, "antigravity": {82, 54, 70, 38}, "opencode": {94, 76, 58, 32}}
	for i, p := range out {
		sample := readings{}
		for j, key := range p.Keys {
			reset := now.Add(time.Duration(150+j*21) * time.Minute)
			if strings.HasSuffix(key, "week") || key == "secondary" {
				reset = now.Add(51 * time.Hour)
			}
			r := measured(examples[p.ID][j%len(examples[p.ID])], reset)
			if key == "primary" {
				r.Label = "5時間枠"
			}
			if key == "secondary" {
				r.Label = "週間枠"
			}
			sample[key] = r
		}
		out[i].Values = sample
	}
	return out
}

func resetText(id string, r reading, now time.Time) string {
	if !r.Valid {
		return ""
	}
	if r.Reset.IsZero() {
		if id == "claude" && r.Percent == 0 {
			return "次回の使用で開始"
		}
		return "リセット時刻不明"
	}
	if id != "claude" {
		return "リセット  " + r.Reset.In(now.Location()).Format("01/02 15:04")
	}
	mins := int(math.Ceil(r.Reset.Sub(now).Minutes()))
	if mins < 0 {
		mins = 0
	}
	if mins >= 1440 {
		return fmt.Sprintf("リセットまで  %d日 %d時間", mins/1440, (mins%1440)/60)
	}
	if mins >= 60 {
		return fmt.Sprintf("リセットまで  %d時間 %d分", mins/60, mins%60)
	}
	return fmt.Sprintf("リセットまで  %d分", mins)
}
