package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Status is shared by the read-only HTTP endpoints, CLI and local history.
// A nil percentage is unknown, while a pointer to zero is a measured zero.
type quotaRow struct {
	Key        string     `json:"key"`
	Name       string     `json:"name"`
	Percent    *float64   `json:"percent"`
	Estimated  bool       `json:"estimated"`
	Reset      *time.Time `json:"reset_at,omitempty"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`
}
type quotaGroup struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Direction   string     `json:"direction"`
	Rows        []quotaRow `json:"rows"`
	FetchedAt   time.Time  `json:"-"`
	FetchFailed bool       `json:"-"`
}
type quotaSnapshot struct {
	Version    int          `json:"version"`
	ObservedAt time.Time    `json:"observed_at"`
	Demo       bool         `json:"demo"`
	Groups     []quotaGroup `json:"groups"`
}

func statusAt(ps []panel, observed, now time.Time, demo bool) quotaSnapshot {
	s := quotaSnapshot{1, observed.UTC(), demo, []quotaGroup{}}
	for _, p := range ps {
		if p.ID == "system" {
			continue // panel-only metrics; not a quota
		}
		dir := "remaining"
		if p.ID == "claude" || p.ID == "codex" {
			dir = "used"
		}
		group := quotaGroup{ID: p.ID, Title: p.Title, Direction: dir, Rows: []quotaRow{}, FetchedAt: p.FetchedAt, FetchFailed: p.FetchFailed}
		for _, key := range p.Keys {
			v := p.valueAt(key, now)
			row := quotaRow{Key: key, Name: rowLabel(p, key)}
			if v.Valid {
				row.Estimated = v.Estimated
				row.Percent = &v.Percent
				if !v.Reset.IsZero() {
					t := v.Reset.UTC()
					row.Reset = &t
				}
				if end := v.expiresAt(); !end.IsZero() {
					t := end.UTC()
					row.ValidUntil = &t
				}
			}
			group.Rows = append(group.Rows, row)
		}
		s.Groups = append(s.Groups, group)
	}
	return s
}

func writeStatus(w io.Writer, s quotaSnapshot, format string, locations ...*time.Location) error {
	if format == "json" {
		return json.NewEncoder(w).Encode(s)
	}
	loc := time.Local
	if len(locations) > 0 {
		loc = locations[0]
	}
	var b strings.Builder
	if format == "text" {
		if s.Demo {
			b.WriteString("DEMO\n")
		}
		b.WriteString(s.ObservedAt.Format("Jan 2 2006 15:04:05 MST") + "\n")
		for _, g := range s.Groups {
			fmt.Fprintf(&b, "%s — %s\n", g.Title, sourceStamp(g.FetchedAt, g.FetchFailed, s.ObservedAt.In(loc)))
			for _, r := range g.Rows {
				value := "N/A"
				if r.Percent != nil {
					percent := *r.Percent
					if g.Direction == "used" {
						percent = 100 - percent
					}
					value = percentText(percent) + "%"
					if r.Estimated {
						value += " est."
					}
				}
				fmt.Fprintf(&b, "  %s  %s", r.Name, value)
				if r.Reset != nil {
					fmt.Fprintf(&b, "  %s", r.Reset.Format("Jan 2 2006 15:04:05 MST"))
				}
				b.WriteByte('\n')
			}
		}
	} else if format == "prometheus" {
		series := []struct{ name, desc string }{{"available", "1 when a quota reading is available."}, {"percent", "Quota percent in the labeled direction."}, {"estimated", "1 when a reading uses a local estimate."}, {"reset_timestamp_seconds", "Quota reset time as Unix seconds."}}
		fmt.Fprintf(&b, "# TYPE quota_panel_observed_timestamp_seconds gauge\nquota_panel_observed_timestamp_seconds %d\n# TYPE quota_panel_demo gauge\nquota_panel_demo %d\n", s.ObservedAt.Unix(), boolInt(s.Demo))
		escape := strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"")
		for _, m := range series {
			fmt.Fprintf(&b, "# HELP quota_panel_%s %s\n# TYPE quota_panel_%s gauge\n", m.name, m.desc, m.name)
			for _, g := range s.Groups {
				for _, r := range g.Rows {
					var value string
					switch m.name {
					case "available":
						value = strconv.Itoa(boolInt(r.Percent != nil))
					case "percent":
						if r.Percent == nil {
							continue
						}
						value = strconv.FormatFloat(*r.Percent, 'g', -1, 64)
					case "estimated":
						if r.Percent == nil {
							continue
						}
						value = strconv.Itoa(boolInt(r.Estimated))
					case "reset_timestamp_seconds":
						if r.Reset == nil {
							continue
						}
						value = strconv.FormatInt(r.Reset.Unix(), 10)
					}
					fmt.Fprintf(&b, "quota_panel_%s{source=\"%s\",key=\"%s\",direction=\"%s\"} %s\n", m.name, escape.Replace(g.ID), escape.Replace(r.Key), g.Direction, value)
				}
			}
		}
	} else {
		return fmt.Errorf("unsupported status format %q", format)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
