package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

type trendKey struct {
	source, row, direction, window string
}

type trendRange struct {
	count    int
	min, max float64
}

type trendValue struct {
	available, estimated bool
	remaining            float64
}

type trendSeries struct {
	reported, estimated trendRange
	missing             int
	last                time.Time
	lastValue           trendValue
	conflict            bool
}

func (s *trendSeries) add(at time.Time, v trendValue) {
	if v.available {
		r := &s.reported
		if v.estimated {
			r = &s.estimated
		}
		if r.count == 0 {
			r.min, r.max = v.remaining, v.remaining
		} else {
			r.min, r.max = min(r.min, v.remaining), max(r.max, v.remaining)
		}
		r.count++
	} else {
		s.missing++
	}
	if at.After(s.last) {
		s.last, s.lastValue, s.conflict = at, v, false
	} else if at.Equal(s.last) && v != s.lastValue {
		s.conflict = true
	}
}

// Preserve small differences between observations; the LCD rounds most values
// to whole percentages. Keep floating point subtraction noise out of reports.
func trendPercent(v float64) string {
	if v == 0 {
		return "0"
	}
	s := strconv.FormatFloat(v, 'g', 8, 64)
	if s == "100" && v < 100 {
		return "<100"
	}
	return s
}

// Consume one exported history record at a time. Memory grows with distinct
// source/row/direction combinations rather than with the number of polls.
func writeTrends(ctx context.Context, w io.Writer, c insightsConfig, now time.Time, days int) error {
	if days < 1 || days > 90 {
		return errors.New("-trend-days must be an integer from 1 to 90")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		return err
	}
	c.RetentionDays = min(days, c.RetentionDays)
	reader, writer := io.Pipe()
	defer reader.Close()
	stop := context.AfterFunc(ctx, func() { reader.CloseWithError(ctx.Err()) })
	defer stop()
	go func() {
		writer.CloseWithError(exportHistory(writer, c, "jsonl", now))
	}()

	series := map[trendKey]*trendSeries{}
	records := json.NewDecoder(reader)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var snap quotaSnapshot
		err := records.Decode(&snap)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		for _, group := range snap.Groups {
			if group.Direction != "used" && group.Direction != "remaining" {
				return fmt.Errorf("Unknown history direction (%q, %q)", group.ID, group.Direction)
			}
			for _, row := range group.Rows {
				key := trendKey{source: group.ID, row: row.Key, direction: group.Direction}
				// Codex uses primary/secondary for different window durations
				// across plans. Its historical label preserves the reported duration.
				if group.ID == "codex" && !strings.EqualFold(row.Name, row.Key) {
					key.window = englishWindowLabel(row.Name)
				}
				s := series[key]
				if s == nil {
					s = &trendSeries{}
					series[key] = s
				}
				var value trendValue
				// Historical validity is evaluated at the original observation.
				// Later resets must not erase readings taken in earlier windows.
				if row.Percent != nil && (row.Reset == nil || snap.ObservedAt.Before(*row.Reset)) &&
					(row.ValidUntil == nil || snap.ObservedAt.Before(*row.ValidUntil)) {
					value = trendValue{true, row.Estimated, *row.Percent}
					if group.Direction == "used" {
						value.remaining = 100 - value.remaining
					}
				}
				s.add(snap.ObservedAt, value)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	keys := make([]trendKey, 0, len(series))
	for k := range series {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b trendKey) int {
		return cmp.Or(cmp.Compare(a.source, b.source), cmp.Compare(a.row, b.row), cmp.Compare(a.direction, b.direction), cmp.Compare(a.window, b.window))
	})
	out := bufio.NewWriter(w)
	fmt.Fprintf(out, "Recorded quota left  %s 00:00 UTC to %s UTC\n", now.UTC().AddDate(0, 0, 1-c.RetentionDays).Format("Jan 2 2006"), now.UTC().Format("Jan 2 2006 15:04:05"))
	fmt.Fprintln(out, "Records may include multiple accounts if you switched accounts.")
	if len(keys) == 0 {
		fmt.Fprintln(out, "No records in this period.")
	}
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		s := series[k]
		direction := "left"
		if k.direction == "used" {
			direction = "used converted to left (100% - used)"
		}
		fmt.Fprintf(out, "\n%q / %q  %s", k.source, k.row, direction)
		if k.source == "codex" {
			period := k.window
			if period == "" {
				period = "window unknown"
			}
			fmt.Fprintf(out, "  %q", period)
		}
		fmt.Fprintln(out)
		fmt.Fprintf(out, "  Valid: %d  N/A: %d\n", s.reported.count+s.estimated.count, s.missing)
		for _, r := range []struct {
			label  string
			values trendRange
		}{{"reported", s.reported}, {"est.", s.estimated}} {
			if r.values.count > 0 {
				fmt.Fprintf(out, "  %s %d  min left %s%%  max left %s%%\n", r.label, r.values.count, trendPercent(r.values.min), trendPercent(r.values.max))
			}
		}
		state := "N/A"
		if s.conflict {
			state = "Conflicting records at the same time"
		} else if v := s.lastValue; v.available {
			state = trendPercent(v.remaining) + "% left (reported)"
			if v.estimated {
				state = trendPercent(v.remaining) + "% left (est.)"
			}
		}
		fmt.Fprintf(out, "  Last record %s  %s\n", s.last.UTC().Format("Jan 2 2006 15:04:05.999999999 UTC"), state)
	}
	return out.Flush()
}

// Normalize legacy Codex window labels without changing stored history.
func englishWindowLabel(label string) string {
	if label == "週間枠" {
		return "weekly"
	}
	for _, suffix := range []struct{ old, new string }{{"時間枠", "h"}, {"分枠", "m"}} {
		if n, ok := strings.CutSuffix(label, suffix.old); ok {
			if _, err := strconv.Atoi(n); err == nil {
				return n + suffix.new
			}
		}
	}
	return label
}
