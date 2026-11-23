package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/pelletier/go-toml/v2"
)

//go:embed trofeo.toml
var defaultTOML []byte

type selection struct {
	Enabled bool     `toml:"enabled"`
	Items   []string `toml:"items"`
}

type claudeConfig struct {
	selection
	ProjectsDir       string `toml:"projects_dir"`
	FiveHourTokens    int64  `toml:"five_hour_tokens"`
	WeeklyTokens      int64  `toml:"weekly_tokens"`
	WeeklyResetAnchor string `toml:"weekly_reset_anchor"`
}

type codexConfig struct {
	selection
	SessionsDir string `toml:"sessions_dir"`
}

type openConfig struct {
	Enabled   bool              `toml:"enabled"`
	Models    []string          `toml:"models"`
	ModelRows map[string]string `toml:"model_rows"`
}

type config struct {
	Interval    int          `toml:"interval_seconds"`
	TrccURL     string       `toml:"trcc_url"`
	DeviceKey   string       `toml:"device_key"`
	Order       []string     `toml:"display_order"`
	Claude      claudeConfig `toml:"claude"`
	Codex       codexConfig  `toml:"codex"`
	Antigravity selection    `toml:"antigravity"`
	OpenCode    openConfig   `toml:"opencode"`
}

func readConfig(path string) (config, error) {
	var c config
	decode := func(b []byte) error {
		return toml.NewDecoder(bytes.NewReader(b)).DisallowUnknownFields().Decode(&c)
	}
	if err := decode(defaultTOML); err != nil {
		return c, err
	}
	optional := path == ""
	if optional {
		path = "trofeo.toml"
	}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := decode(b); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	case !optional || !errors.Is(err, os.ErrNotExist):
		return c, err
	}
	if c.Interval < 1 || int64(c.Interval) > int64((time.Duration(1<<63-1))/time.Second) {
		return c, errors.New("interval_seconds must be a positive duration in seconds")
	}
	u, err := url.Parse(c.TrccURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("trcc_url must be an HTTP(S) base URL")
	}
	c.TrccURL = strings.TrimRight(c.TrccURL, "/")
	bound := map[string]bool{}
	for model, id := range c.OpenCode.ModelRows {
		if strings.TrimSpace(model) == "" || model == "@shared" || !strings.HasPrefix(id, "quota-providers.custom.") || bound[id] {
			return c, fmt.Errorf("opencode.model_rows: expected a distinct custom quota row id for %q", model)
		}
		bound[id] = true
	}
	if c.Claude.FiveHourTokens < 0 || c.Claude.WeeklyTokens < 0 {
		return c, errors.New("Claude token budgets must be nonnegative")
	}
	if c.Claude.WeeklyResetAnchor != "" {
		if _, err := time.Parse(time.RFC3339, c.Claude.WeeklyResetAnchor); err != nil {
			return c, fmt.Errorf("weekly_reset_anchor: %w", err)
		}
	}
	for _, group := range []struct {
		name         string
		items, valid []string
	}{
		{"display_order", c.Order, []string{"claude", "codex", "antigravity", "opencode"}},
		{"claude.items", c.Claude.Items, []string{"5h", "week"}},
		{"codex.items", c.Codex.Items, []string{"primary", "secondary"}},
		{"antigravity.items", c.Antigravity.Items, []string{"gemini.5h", "gemini.week", "claude_gpt.5h", "claude_gpt.week"}},
		{"opencode.models", c.OpenCode.Models, nil},
	} {
		seen := map[string]bool{}
		for _, item := range group.items {
			if strings.TrimSpace(item) == "" || strings.IndexFunc(item, unicode.IsControl) >= 0 || seen[item] || (group.valid != nil && !slices.Contains(group.valid, item)) {
				return c, fmt.Errorf("%s: invalid or duplicate item %q", group.name, item)
			}
			seen[item] = true
		}
	}
	return c, nil
}

func logPath(override, env, fallback, leaf string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if override != "" {
		if strings.HasPrefix(override, "~/") {
			return filepath.Join(home, override[2:]), nil
		}
		return override, nil
	}
	base := os.Getenv(env)
	if base == "" {
		base = filepath.Join(home, fallback)
	}
	return filepath.Join(base, leaf), nil
}
