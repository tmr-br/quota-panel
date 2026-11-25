package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeRunner replays canned command output keyed by the full command line.
func fakeRunner(out map[string]string) commandRunner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		line := strings.Join(append([]string{name}, args...), " ")
		s, ok := out[line]
		if !ok {
			return nil, errors.New(line + ": not available")
		}
		return []byte(s), nil
	}
}

const (
	iostatOut = `      cpu    load average
 us sy id   1m   5m   15m
 27 18 54  11.21 25.91 68.09
 22 27 51  11.35 25.70 67.77
`
	vmStatOut = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                     4080.
Pages active:                                 157543.
Pages wired down:                             228435.
Pages purgeable:                                   4.
"Translation faults":                     3014978677.
Anonymous pages:                              195980.
Pages occupied by compressor:                 462381.
`
	dfOut = `Filesystem   1024-blocks      Used Available Capacity  Mounted on
/dev/disk3s5   482797652 425755496  13841440    97%    /System/Volumes/Data
`
)

func sampleCommands() map[string]string {
	return map[string]string{
		"iostat -n 0 -c 2 -w 1":         iostatOut,
		"sysctl -n hw.memsize":          "17179869184\n",
		"vm_stat":                       vmStatOut,
		"sysctl -n vm.swapusage":        "total = 16384.00M  used = 15610.94M  free = 773.06M  (encrypted)\n",
		"df -k -P /System/Volumes/Data": dfOut,
	}
}

func TestFetchSystemParsesMacOSCommands(t *testing.T) {
	got, err := fetchSystem(context.Background(), []string{"cpu", "memory", "swap", "disk"}, fakeRunner(sampleCommands()))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key     string
		percent string
		detail  string
	}{
		{"cpu", "49", "user 22% · sys 27%"}, // second sample, 100 - idle
		{"memory", "85", "13.5 of 16.0 GB used"},
		{"swap", "95", "15.2 of 16.0 GB swapped"},
		{"disk", "3", "14.2 GB free of 494 GB"},
	} {
		r := got[tc.key]
		if !r.Valid || percentText(r.Percent) != tc.percent || r.Detail != tc.detail {
			t.Errorf("%s = %+v, want %s%% %q", tc.key, r, tc.percent, tc.detail)
		}
	}
	if !systemWarn("swap", got["swap"]) || !systemWarn("disk", got["disk"]) || systemWarn("cpu", got["cpu"]) {
		t.Error("warning thresholds")
	}
}

// A command that fails or prints something unexpected leaves only its own
// row unavailable, and the error is reported rather than shown as 0%.
func TestFetchSystemFailuresAreUnavailable(t *testing.T) {
	cmds := sampleCommands()
	delete(cmds, "vm_stat")
	cmds["sysctl -n vm.swapusage"] = "garbage"
	cmds["iostat -n 0 -c 2 -w 1"] = "      cpu    load average\n us sy id   1m   5m   15m\n"
	delete(cmds, "df -k -P /System/Volumes/Data")
	cmds["df -k -P /"] = dfOut // falls back to the root volume
	got, err := fetchSystem(context.Background(), []string{"cpu", "memory", "swap", "disk"}, fakeRunner(cmds))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{"cpu", "memory", "swap"} {
		if got[key].Valid {
			t.Errorf("%s should be unavailable: %+v", key, got[key])
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not name %s: %v", key, err)
		}
	}
	if !got["disk"].Valid {
		t.Error("disk fallback failed")
	}

	p := panel{ID: "system", Title: "System", Keys: []string{"cpu"}, Values: got}
	if v := p.valueAt("cpu", time.Now()); v.Valid || resetText("system", v, time.Now()) != "" {
		t.Fatal("unavailable CPU must not carry a value or detail")
	}
}

func TestSwapWithoutSwapFilesIsMeasuredZero(t *testing.T) {
	cmds := map[string]string{"sysctl -n vm.swapusage": "total = 0.00M  used = 0.00M  free = 0.00M  (encrypted)\n"}
	got, err := fetchSystem(context.Background(), []string{"swap"}, fakeRunner(cmds))
	if err != nil || !got["swap"].Valid || got["swap"].Percent != 0 || got["swap"].Detail != "no swap in use" {
		t.Fatal(got, err)
	}
}

// System metrics are panel-only: status, history and alerts see quotas alone.
func TestSystemExcludedFromQuotaStatus(t *testing.T) {
	now := time.Now()
	ps := []panel{
		{ID: "codex", Title: "Codex", Keys: []string{"primary"}, Values: readings{"primary": measured(10, time.Time{})}},
		{ID: "system", Title: "System", Keys: []string{"cpu"}, Values: readings{"cpu": measured(50, time.Time{})}},
	}
	s := statusAt(ps, now, now, false)
	if len(s.Groups) != 1 || s.Groups[0].ID != "codex" {
		t.Fatalf("%+v", s.Groups)
	}
}

func TestSystemConfig(t *testing.T) {
	c, err := readConfig("trofeo.toml")
	if err != nil {
		t.Fatal(err)
	}
	ps := panels(c)
	if last := ps[len(ps)-1]; last.ID != "system" || len(last.Keys) != 4 || rowLabel(last, "disk") != "Storage free" {
		t.Fatalf("%+v", last)
	}
	demo := demoPanels(c, time.Now())
	if v := demo[len(demo)-1].Values["swap"]; !v.Valid || v.Detail == "" {
		t.Fatalf("%+v", v)
	}
}
