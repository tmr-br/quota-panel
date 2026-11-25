package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type insightsConfig struct {
	HistoryDir     string   `toml:"history_dir"`
	RetentionDays  int      `toml:"retention_days"`
	AlertRemaining *float64 `toml:"alert_remaining_percent"`
}

func (c insightsConfig) validate() error {
	if c.RetentionDays < 1 || c.RetentionDays > 90 {
		return errors.New("insights.retention_days must be between 1 and 90")
	}
	if n := c.AlertRemaining; n != nil && (math.IsNaN(*n) || math.IsInf(*n, 0) || *n < 0 || *n > 100) {
		return errors.New("insights.alert_remaining_percent must be between 0 and 100")
	}
	return nil
}

type alertKey struct {
	source, row string
}

type insights struct {
	low       map[alertKey]bool
	threshold float64
}

func (s *insights) observe(c config, snapshot quotaSnapshot) ([]string, error) {
	var notices []string
	if snapshot.Demo {
		return nil, nil
	}
	if c.Insights.AlertRemaining == nil {
		s.low = nil
	} else {
		threshold := *c.Insights.AlertRemaining
		if s.low == nil || s.threshold != threshold {
			s.low = map[alertKey]bool{}
			s.threshold = threshold
		}
		next := map[alertKey]bool{}
		for _, g := range snapshot.Groups {
			for _, r := range g.Rows {
				key := alertKey{source: g.ID, row: r.Key}
				was := s.low[key]
				if r.Percent == nil {
					next[key] = was
					continue
				}
				remaining := *r.Percent
				if g.Direction == "used" {
					remaining = 100 - remaining
				}
				low := remaining <= threshold
				if low != was {
					event := "Low quota"
					if !low {
						event = "Quota recovered"
					}
					if r.Estimated {
						event += " (est.)"
					}
					notices = append(notices, fmt.Sprintf("%s %s/%s %s%% left", event, g.Title, r.Name, percentText(remaining)))
				}
				next[key] = low
			}
		}
		s.low = next
	}
	if c.Insights.HistoryDir == "" {
		return notices, nil
	}
	return notices, appendHistory(c.Insights, snapshot)
}

func historyDir(c insightsConfig) (string, error) {
	if c.HistoryDir == "" {
		return "", errors.New("set insights.history_dir to enable history")
	}
	path := c.HistoryDir
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
	}
	return filepath.Join(path, "quota-panel-v1"), nil
}

// Each UTC day keeps at most two 4 MiB segments. Append and rotation share a
// lock across processes. Exports keep descriptors at a known size so client
// writes never hold up a collector.
const historySegment = 4 << 20

var historyName = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}\.[01]\.jsonl$`)

func withHistoryLock(dir string, op int, fn func() error) error {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("invalid history lock file")
	}
	if err = syscall.Flock(int(f.Fd()), op); err != nil {
		return err
	}
	return fn()
}
func historyCutoff(now time.Time, days int) string {
	return now.UTC().AddDate(0, 0, 1-days).Format("2006-01-02")
}

func appendHistory(c insightsConfig, s quotaSnapshot) error {
	if s.Demo {
		return nil
	}
	if s.Version != 1 || s.ObservedAt.IsZero() {
		return errors.New("invalid quota history snapshot")
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(raw) > historySegment {
		return errors.New("history snapshot exceeds segment size")
	}
	dir, err := historyDir(c)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return withHistoryLock(dir, syscall.LOCK_EX, func() error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		cutoff := historyCutoff(s.ObservedAt, c.RetentionDays)
		for _, e := range entries {
			if historyName.MatchString(e.Name()) && e.Name()[:10] < cutoff && e.Type().IsRegular() {
				if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
					return err
				}
			}
		}
		stem := filepath.Join(dir, s.ObservedAt.UTC().Format("2006-01-02"))
		path := stem + ".1.jsonl"
		return appendSegment(path, stem+".0.jsonl", raw)
	})
}

// Find the last intact record without scanning an entire healthy segment.
// An intact record whose newline was lost is retained. Readers use the same
// boundary as writers so a repaired append cannot change an open export.
func historyEnd(f *os.File) (end int64, needsNewline bool, err error) {
	st, err := f.Stat()
	if err != nil {
		return 0, false, err
	}
	if !st.Mode().IsRegular() || st.Size() > historySegment {
		return 0, false, fmt.Errorf("invalid history segment %s", filepath.Base(f.Name()))
	}
	size := st.Size()
	for n := min(size, int64(4096)); n > 0; n = min(size, 2*n) {
		tail := make([]byte, n)
		if _, err := f.ReadAt(tail, size-n); err != nil {
			return 0, false, err
		}
		for len(tail) != 0 {
			end := len(tail)
			content := bytes.TrimSuffix(tail, []byte{'\n'})
			start := bytes.LastIndexByte(content, '\n') + 1
			if start == 0 && n < size {
				break
			} // The record began before this buffer.
			if record := content[start:]; len(record) > 0 {
				var snap quotaSnapshot
				if err := json.Unmarshal(record, &snap); err == nil {
					if snap.Version > 1 {
						return 0, false, fmt.Errorf("unsupported history version %d", snap.Version)
					}
					if snap.Version == 1 && !snap.ObservedAt.IsZero() {
						return size - n + int64(end), end == len(content), nil
					}
				}
			}
			tail = tail[:start]
		}
		if n == size {
			break
		}
	}
	return 0, false, nil
}

func appendSegment(path, previous string, raw []byte) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	end, needsNewline, err := historyEnd(f)
	if err != nil {
		return err
	}
	// Preserve the valid prefix before rotation: the damaged tail must
	// neither become part of the next record nor displace an older segment.
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() != end {
		if err := f.Truncate(end); err != nil {
			return err
		}
		log.Printf("history: repaired %s, removed %d damaged trailing bytes", filepath.Base(path), st.Size()-end)
	}
	size := end + int64(len(raw))
	if needsNewline {
		size++
	}
	dest := f
	if size > historySegment {
		if err := os.Rename(path, previous); err != nil {
			return err
		}
		dest, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, dest.Close()) }()
	} else if needsNewline {
		if _, err := f.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	_, err = dest.Write(raw)
	return err
}

type historyPart struct {
	f *os.File
	n int64
}

func exportHistory(w io.Writer, c insightsConfig, format string, now time.Time) error {
	if format != "csv" && format != "json" && format != "jsonl" {
		return fmt.Errorf("unknown history format %q", format)
	}
	dir, err := historyDir(c)
	if err != nil {
		return err
	}
	var parts []historyPart
	defer func() {
		for _, p := range parts {
			p.f.Close()
		}
	}()
	cutoff := historyCutoff(now, c.RetentionDays)
	today := now.UTC().Format("2006-01-02")
	err = withHistoryLock(dir, syscall.LOCK_SH, func() error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if !historyName.MatchString(name) || name[:10] < cutoff || name[:10] > today {
				continue
			}
			f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			end, _, err := historyEnd(f)
			if err != nil {
				f.Close()
				return err
			}
			parts = append(parts, historyPart{f, end})
		}
		return nil
	})
	if err != nil {
		if _, statErr := os.Stat(dir); !errors.Is(err, os.ErrNotExist) || !errors.Is(statErr, os.ErrNotExist) {
			return err
		}
	}
	csvw := csv.NewWriter(w)
	if format == "csv" {
		if err := csvw.Write([]string{"observed_at", "source", "key", "direction", "percent", "estimated", "reset_at"}); err != nil {
			return err
		}
	}
	if format == "json" {
		if _, err := io.WriteString(w, "["); err != nil {
			return err
		}
	}
	first := true
	for _, part := range parts {
		scan := bufio.NewScanner(io.LimitReader(part.f, part.n))
		scan.Buffer(make([]byte, 4096), historySegment+1)
		for scan.Scan() {
			var s quotaSnapshot
			if err := json.Unmarshal(scan.Bytes(), &s); err != nil || s.Version != 1 || s.ObservedAt.IsZero() {
				return fmt.Errorf("invalid history record in %s", filepath.Base(part.f.Name()))
			}
			if s.Demo || s.ObservedAt.After(now) || s.ObservedAt.UTC().Format("2006-01-02") < cutoff {
				continue
			}
			if format == "csv" {
				for _, g := range s.Groups {
					for _, r := range g.Rows {
						value, reset := "", ""
						if r.Percent != nil {
							value = strconv.FormatFloat(*r.Percent, 'g', -1, 64)
						}
						if r.Reset != nil {
							reset = r.Reset.Format(time.RFC3339)
						}
						key := r.Key
						if text := strings.TrimLeft(key, " "); text != "" && strings.IndexByte("=+@-\t\r\n", text[0]) >= 0 {
							key = "'" + key
						}
						if err := csvw.Write([]string{s.ObservedAt.Format(time.RFC3339Nano), g.ID, key, g.Direction, value, strconv.FormatBool(r.Estimated), reset}); err != nil {
							return err
						}
					}
				}
			} else {
				if format == "json" && !first {
					if _, err := io.WriteString(w, ","); err != nil {
						return err
					}
				}
				if err := json.NewEncoder(w).Encode(s); err != nil {
					return err
				}
			}
			first = false
		}
		if err := scan.Err(); err != nil {
			return err
		}
	}
	if format == "json" {
		_, err = io.WriteString(w, "]\n")
		return err
	}
	csvw.Flush()
	return csvw.Error()
}
