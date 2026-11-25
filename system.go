package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// System metrics for the Mac running the panel. They are display-only: the
// quota status, history and alerts never see them (see statusAt).
//
// Every value comes from a standard macOS command so tests can substitute
// the output. A command that fails or prints something unexpected leaves its
// row unavailable (N/A); it is never shown as a measured 0%.

type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// systemRunner runs the System column's commands; tests replace it.
var systemRunner commandRunner = runCommand

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

const (
	gib = 1 << 30
	gb  = 1e9
)

func fetchSystem(ctx context.Context, items []string, run commandRunner) (readings, error) {
	out := readings{}
	var problems []error
	for _, key := range items {
		var r reading
		var err error
		switch key {
		case "cpu":
			r, err = cpuReading(ctx, run)
		case "memory":
			r, err = memoryReading(ctx, run)
		case "swap":
			r, err = swapReading(ctx, run)
		case "disk":
			r, err = diskReading(ctx, run)
		}
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", key, err))
			continue
		}
		out[key] = r
	}
	return out, errors.Join(problems...)
}

// valid builds a reading only from an in-range percentage.
func valid(p float64, detail string) (reading, error) {
	r := measured(p, time.Time{})
	if !r.Valid {
		return r, fmt.Errorf("out of range: %v", p)
	}
	r.Detail = detail
	return r, nil
}

// iostat's first sample covers time since boot, so read the last of two
// one-second samples. It is far cheaper than top (≈0 vs ≈1.4 s of CPU per
// call), which matters when the System column refreshes every few seconds.
func cpuReading(ctx context.Context, run commandRunner) (reading, error) {
	b, err := run(ctx, "iostat", "-n", "0", "-c", "2", "-w", "1")
	if err != nil {
		return reading{}, err
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var fields []string
	if len(lines) >= 4 { // two header lines, then one line per sample
		fields = strings.Fields(lines[len(lines)-1])
	}
	if len(fields) < 3 {
		return reading{}, errors.New("no CPU sample in iostat output")
	}
	var v [3]float64
	for i := range v {
		if v[i], err = strconv.ParseFloat(fields[i], 64); err != nil {
			return reading{}, fmt.Errorf("iostat sample %q", lines[len(lines)-1])
		}
	}
	return valid(100-v[2], fmt.Sprintf("user %s%% · sys %s%%", percentText(v[0]), percentText(v[1])))
}

// Used memory follows Activity Monitor: app memory (anonymous minus
// purgeable) + wired + compressed.
func memoryReading(ctx context.Context, run commandRunner) (reading, error) {
	b, err := run(ctx, "sysctl", "-n", "hw.memsize")
	if err != nil {
		return reading{}, err
	}
	total, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil || total <= 0 {
		return reading{}, fmt.Errorf("hw.memsize %q", strings.TrimSpace(string(b)))
	}
	b, err = run(ctx, "vm_stat")
	if err != nil {
		return reading{}, err
	}
	pageSize, pages, err := parseVMStat(b)
	if err != nil {
		return reading{}, err
	}
	need := func(name string) (float64, error) {
		v, ok := pages[name]
		if !ok {
			return 0, fmt.Errorf("vm_stat lacks %q", name)
		}
		return v, nil
	}
	wired, err := need("Pages wired down")
	if err != nil {
		return reading{}, err
	}
	compressed, err := need("Pages occupied by compressor")
	if err != nil {
		return reading{}, err
	}
	app, ok := pages["Anonymous pages"]
	if ok {
		app -= pages["Pages purgeable"]
	} else if app, err = need("Pages active"); err != nil {
		return reading{}, err
	}
	used := (max(app, 0) + wired + compressed) * pageSize
	return valid(used/total*100, fmt.Sprintf("%s of %s GB used", gbText(used/gib), gbText(total/gib)))
}

var pageSizeLine = regexp.MustCompile(`page size of (\d+) bytes`)

func parseVMStat(b []byte) (float64, map[string]float64, error) {
	m := pageSizeLine.FindSubmatch(b)
	if m == nil {
		return 0, nil, errors.New("vm_stat lacks a page size")
	}
	size, _ := strconv.ParseFloat(string(m[1]), 64)
	pages := map[string]float64{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		if v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(value), "."), 64); err == nil {
			pages[strings.TrimSpace(strings.Trim(name, `"`))] = v
		}
	}
	return size, pages, nil
}

var swapLine = regexp.MustCompile(`total = ([\d.]+)M\s+used = ([\d.]+)M`)

func swapReading(ctx context.Context, run commandRunner) (reading, error) {
	b, err := run(ctx, "sysctl", "-n", "vm.swapusage")
	if err != nil {
		return reading{}, err
	}
	m := swapLine.FindSubmatch(b)
	if m == nil {
		return reading{}, fmt.Errorf("vm.swapusage %q", strings.TrimSpace(string(b)))
	}
	total, _ := strconv.ParseFloat(string(m[1]), 64)
	used, _ := strconv.ParseFloat(string(m[2]), 64)
	if total == 0 {
		// macOS creates swap files on demand; none means nothing is swapped.
		return valid(0, "no swap in use")
	}
	return valid(used/total*100, fmt.Sprintf("%s of %s GB swapped", gbText(used/1024), gbText(total/1024)))
}

// The writable APFS data volume holds user files; / is the sealed system
// snapshot. Free space is shown as a share of the container size.
func diskReading(ctx context.Context, run commandRunner) (reading, error) {
	b, err := run(ctx, "df", "-k", "-P", "/System/Volumes/Data")
	if err != nil {
		if b, err = run(ctx, "df", "-k", "-P", "/"); err != nil {
			return reading{}, err
		}
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 2 {
		return reading{}, errors.New("df printed no volume")
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return reading{}, fmt.Errorf("df line %q", lines[len(lines)-1])
	}
	total, err1 := strconv.ParseFloat(f[1], 64)
	avail, err2 := strconv.ParseFloat(f[3], 64)
	if err1 != nil || err2 != nil || total <= 0 {
		return reading{}, fmt.Errorf("df line %q", lines[len(lines)-1])
	}
	return valid(avail/total*100, fmt.Sprintf("%s GB free of %s GB", gbText(avail*1024/gb), gbText(total*1024/gb)))
}

func gbText(v float64) string {
	if v < 100 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strconv.FormatFloat(v, 'f', 0, 64)
}

// systemWarn reports a reading that needs attention: heavy CPU, memory or
// swap use, or little free storage.
func systemWarn(key string, r reading) bool {
	if !r.Valid {
		return false
	}
	if key == "disk" {
		return r.Percent <= 10
	}
	return r.Percent >= 80
}

func hostName() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	h, _, _ = strings.Cut(h, ".")
	return h
}
