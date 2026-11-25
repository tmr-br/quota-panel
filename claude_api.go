package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Claude Code keeps its claude.ai sign-in in the macOS Keychain, or in
// .credentials.json elsewhere. The token is only sent to Anthropic's usage
// endpoint and is never logged, stored or refreshed here: refreshing would
// rotate the token Claude Code itself depends on.
const (
	claudeUsageURL      = "https://api.anthropic.com/api/oauth/usage"
	claudeKeychainName  = "Claude Code-credentials"
	claudeRequiredScope = "user:profile"
)

type claudeCredential struct {
	OAuth *struct {
		AccessToken string   `json:"accessToken"`
		ExpiresAt   *float64 `json:"expiresAt"` // Unix milliseconds
		Scopes      []string `json:"scopes"`
	} `json:"claudeAiOauth"`
}

type claudeUsageWindow struct {
	Utilization *float64 `json:"utilization"` // percent used, 0-100
	ResetsAt    *string  `json:"resets_at"`
}

type claudeUsageResponse struct {
	FiveHour *claudeUsageWindow `json:"five_hour"`
	SevenDay *claudeUsageWindow `json:"seven_day"`
}

// Tests replace these to avoid touching the real Keychain or network.
var (
	readClaudeCredential = loadClaudeCredential
	claudeUsageEndpoint  = claudeUsageURL
)

func loadClaudeCredential(ctx context.Context) ([]byte, error) {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".claude")
	}
	b, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if err == nil || !errors.Is(err, os.ErrNotExist) || runtime.GOOS != "darwin" {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("no Claude Code sign-in found; run `claude` and log in")
		}
		return b, err
	}
	out, err := exec.CommandContext(ctx, "security", "find-generic-password", "-s", claudeKeychainName, "-w").Output()
	if err != nil {
		return nil, errors.New("no Claude Code sign-in in the Keychain; run `claude` and log in")
	}
	return out, nil
}

func fetchClaudeUsageAPI(ctx context.Context, now time.Time) (readings, error) {
	raw, err := readClaudeCredential(ctx)
	if err != nil {
		return nil, err
	}
	var cred claudeCredential
	if err := json.Unmarshal(raw, &cred); err != nil || cred.OAuth == nil || cred.OAuth.AccessToken == "" {
		return nil, errors.New("unreadable Claude Code sign-in")
	}
	if cred.OAuth.ExpiresAt != nil && !now.Before(time.UnixMilli(int64(*cred.OAuth.ExpiresAt))) {
		return nil, errors.New("Claude Code sign-in expired; start `claude` once to refresh it")
	}
	if len(cred.OAuth.Scopes) > 0 && !slices.Contains(cred.OAuth.Scopes, claudeRequiredScope) {
		return nil, fmt.Errorf("Claude Code sign-in lacks the %s scope; run `claude` and log in again", claudeRequiredScope)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.OAuth.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "quota-panel/"+strings.Fields(version())[0])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Claude usage API: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, errors.New("Claude usage API rejected the sign-in (401); start `claude` once to refresh it")
	default:
		return nil, fmt.Errorf("Claude usage API: HTTP %d", resp.StatusCode)
	}
	var body claudeUsageResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("Claude usage API: %w", err)
	}
	out := readings{}
	for key, w := range map[string]*claudeUsageWindow{"5h": body.FiveHour, "week": body.SevenDay} {
		if w == nil || w.Utilization == nil {
			continue
		}
		r := reading{Valid: true, Percent: *w.Utilization}
		if w.ResetsAt != nil {
			reset, err := time.Parse(time.RFC3339Nano, *w.ResetsAt)
			if err != nil {
				return nil, fmt.Errorf("Claude usage API: invalid resets_at %q", *w.ResetsAt)
			}
			r.Reset = reset
		}
		if !r.Reset.IsZero() && !r.Reset.After(now) {
			continue
		}
		out[key] = r
	}
	return out, nil
}
