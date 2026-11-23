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
		return errors.New("-trend-days は 1〜90 の整数で指定してください")
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
				return fmt.Errorf("履歴の記録方向が不明です (%q, %q)", group.ID, group.Direction)
			}
			for _, row := range group.Rows {
				key := trendKey{source: group.ID, row: row.Key, direction: group.Direction}
				// Codex uses primary/secondary for different window durations
				// across plans. Its historical label preserves the reported duration.
				if group.ID == "codex" && !strings.EqualFold(row.Name, row.Key) {
					key.window = row.Name
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
	fmt.Fprintf(out, "残量の記録値　%s 00:00 UTC ～ %s UTC\n", historyCutoff(now, c.RetentionDays), now.UTC().Format("2006-01-02 15:04:05"))
	fmt.Fprintln(out, "アカウントを切り替えた場合、同じ項目には切り替え前後の記録値が含まれます。")
	if len(keys) == 0 {
		fmt.Fprintln(out, "対象期間の記録はありません。")
	}
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		s := series[k]
		direction := "残量"
		if k.direction == "used" {
			direction = "使用率から換算（100% − 使用率）"
		}
		fmt.Fprintf(out, "\n%q / %q　%s", k.source, k.row, direction)
		if k.source == "codex" {
			period := k.window
			if period == "" {
				period = "期間不明"
			}
			fmt.Fprintf(out, "　%q", period)
		}
		fmt.Fprintln(out)
		fmt.Fprintf(out, "  有効 %d件　取得不能 %d件\n", s.reported.count+s.estimated.count, s.missing)
		for _, r := range []struct {
			label  string
			values trendRange
		}{{"実測", s.reported}, {"推定", s.estimated}} {
			if r.values.count > 0 {
				fmt.Fprintf(out, "  %s %d件　最小残量 %s%%　最大残量 %s%%\n", r.label, r.values.count, trendPercent(r.values.min), trendPercent(r.values.max))
			}
		}
		state := "取得不能"
		if s.conflict {
			state = "同時刻の記録が不一致"
		} else if v := s.lastValue; v.available {
			state = "残量 " + trendPercent(v.remaining) + "%（実測）"
			if v.estimated {
				state = "残量 " + trendPercent(v.remaining) + "%（推定）"
			}
		}
		fmt.Fprintf(out, "  最終記録 %s　%s\n", s.last.UTC().Format(time.RFC3339Nano), state)
	}
	return out.Flush()
}
