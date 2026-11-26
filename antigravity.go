package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Match a version report, never an update notice or a dependency version.
var agyVersionLine = regexp.MustCompile(`(?i)^(?:(?:google[ ]+)?(?:agy|antigravity)(?:[ -]+cli)?(?:[ ]+\(agy\))?[ ]*)?(?:version[ ]*)?[:=]?[ ]*v?([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9a-z.-]+)?(?:\+[0-9a-z.-]+)?)(?:[ ]+\([^()]*\))?$`)
var versionNumber = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`)

func reportedVersion(raw []byte) (string, error) {
	var version string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		m := agyVersionLine.FindStringSubmatch(strings.TrimSpace(line))
		if len(m) == 0 {
			if versionNumber.MatchString(line) {
				return "", errors.New("ambiguous agy version report")
			}
			continue
		}
		candidate := "v" + m[1]
		if !semver.IsValid(candidate) || version != "" || len(versionNumber.FindAllString(line, 2)) != 1 {
			return "", errors.New("ambiguous or invalid agy version")
		}
		version = candidate
	}
	if version == "" {
		return "", errors.New("unrecognized agy --version output")
	}
	return version, nil
}

type agyBinary struct {
	path, version string
	info          os.FileInfo
}

func inspectAgy(ctx context.Context) (agyBinary, error) {
	var tool agyBinary
	path, err := exec.LookPath("agy")
	if err != nil {
		return tool, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return tool, err
	}
	before, err := os.Stat(path)
	if err != nil {
		return tool, err
	}
	b, err := commandOutput(ctx, 5*time.Second, path, "--version")
	if err != nil {
		return tool, err
	}
	v, err := reportedVersion(b)
	if err != nil {
		return tool, err
	}
	return agyBinary{path, v, before}, nil
}

type agyFallback func(context.Context, []string, time.Time) (readings, error)

func fetchAntigravity(ctx context.Context, keys []string, now time.Time, fallback agyFallback) (readings, error) {
	tool, err := inspectAgy(ctx)
	if err != nil || semver.Compare(tool.version, "v1.1.11") < 0 {
		values, rpcErr := fallback(ctx, keys, now)
		if rpcErr != nil {
			if err == nil {
				err = fmt.Errorf("agy %s requires local RPC", tool.version)
			}
			return values, errors.Join(err, rpcErr)
		}
		return values, nil
	}
	// Use the same resolved executable. Re-check on the next poll if an
	// installer replaced it while --version was running.
	cur, err := os.Stat(tool.path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(cur, tool.info) || cur.Size() != tool.info.Size() || !cur.ModTime().Equal(tool.info.ModTime()) {
		return nil, errors.New("agy changed during version check")
	}
	b, err := commandOutput(ctx, 40*time.Second, tool.path, "-p", "/usage", "--output-format", "json")
	if err != nil {
		return nil, err
	}
	var report struct {
		Status  string
		Command struct {
			Name string
			Data agSummary
		}
	}
	if err = json.Unmarshal(b, &report); err != nil {
		return nil, err
	}
	if !strings.EqualFold(report.Status, "SUCCESS") || report.Command.Name != "usage" {
		return nil, errors.New("agy response is not a successful usage command")
	}
	return report.Command.Data.readings(keys, now)
}

type agBucket struct {
	ID          string `json:"id"`
	BucketID    string `json:"bucketId"`
	Disabled    bool
	Fraction    *float64 `json:"remainingFraction"`
	CLIFraction *float64 `json:"remaining_fraction"`
	Reset       string   `json:"resetTime"`
	CLIReset    string   `json:"reset_time"`
	Remaining   struct {
		RemainingFraction *float64
		Case              string
		Value             *float64
	}
}
type agSummary struct {
	Groups []struct{ Buckets []agBucket }
}

func (b agBucket) read(now time.Time) reading {
	if b.Disabled {
		return reading{}
	}
	p := b.Fraction
	if p == nil {
		p = b.CLIFraction
	}
	if p == nil {
		p = b.Remaining.RemainingFraction
	}
	if p == nil && b.Remaining.Case == "remainingFraction" {
		p = b.Remaining.Value
	}
	at := b.Reset
	if at == "" {
		at = b.CLIReset
	}
	reset, err := time.Parse(time.RFC3339Nano, at)
	if err != nil || p == nil {
		return reading{}
	}
	// agy can report a completely unused bucket with the Unix epoch as
	// its unset reset time. Preserve the measured balance, not a fake date.
	if *p == 1 && reset.Equal(time.Unix(0, 0)) {
		return measured(100, time.Time{})
	}
	if !reset.After(now) {
		return reading{}
	}
	return measured(*p*100, reset)
}

var bucketKey = map[string]string{
	"gemini-5h": "gemini.5h", "gemini-weekly": "gemini.week",
	"3p-5h": "claude_gpt.5h", "3p-weekly": "claude_gpt.week",
}

func (summary agSummary) readings(keys []string, now time.Time) (readings, error) {
	out := readings{}
	for _, g := range summary.Groups {
		for _, b := range g.Buckets {
			id := b.BucketID
			if id == "" {
				id = b.ID
			}
			k, ok := bucketKey[id]
			if !ok {
				continue
			}
			if _, duplicate := out[k]; duplicate {
				out[k] = reading{}
				continue
			}
			out[k] = b.read(now)
		}
	}
	return out, missingReadings(keys, out)
}

func missingReadings(keys []string, out readings) error {
	var missing []string
	for _, k := range keys {
		if !out[k].Valid {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("missing, expired or ambiguous buckets: %s", strings.Join(missing, ", "))
}
