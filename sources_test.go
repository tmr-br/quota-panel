package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeNestedLogsDeduplicationBudgetsAndTruncatedWrites(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "account", "session", "subagents", "agent.jsonl")
	now := time.Date(2026, 11, 23, 15, 20, 0, 0, time.UTC)
	c := claudeConfig{selection: selection{Items: []string{"5h", "week"}}, ProjectsDir: dir, FiveHourTokens: 100, WeeklyTokens: 1000, WeeklyResetAnchor: "2026-11-26T12:00:00Z"}
	line := func(at, id string, n int) string {
		return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":"req","message":{"id":%q,"model":"opus","usage":{"input_tokens":%d,"output_tokens":3,"cache_read_input_tokens":4,"cache_creation_input_tokens":2}}}`+"\n", at, id, n)
	}
	history := line("2026-11-22T11:00:00Z", "old", 1)
	recent := line("2026-11-23T13:57:00Z", "current", 11)
	writeFixture(t, filepath.Join(dir, "one.jsonl"), history+recent)
	writeFixture(t, file, recent)
	cache := logCache[claudeEvent]{}
	get := func() (readings, error) { return fetchClaude(context.Background(), c, cache, now) }
	v, err := get()
	if err != nil || !v["5h"].Valid || v["5h"].Percent != 20 || v["5h"].Reset.Hour() != 18 || v["week"].Percent != 3 {
		t.Fatal(v, err)
	}
	writeFixture(t, file, recent+`{"type":"assistant"`)
	if v, err = get(); err == nil || v["5h"].Valid {
		t.Fatal("torn record counted", v, err)
	}
	writeFixture(t, file, recent+line("2026-11-23T14:05:00Z", "second", 1))
	if v, err = get(); err != nil || v["5h"].Percent != 30 {
		t.Fatal("recovery", v, err)
	}
	c.FiveHourTokens = 0
	if v, err = get(); err == nil || v["5h"].Valid || !v["week"].Valid {
		t.Fatal("absent budget", v, err)
	}
	c.FiveHourTokens = 100
	c.WeeklyResetAnchor = ""
	if v, err = get(); err == nil || v["week"].Valid || !v["5h"].Valid {
		t.Fatal("absent anchor", v, err)
	}
	now = now.Add(6 * time.Hour)
	v, _ = get()
	if !v["5h"].Valid || v["5h"].Percent != 0 || !v["5h"].Reset.IsZero() {
		t.Fatal("inactive block", v)
	}
	c.ProjectsDir = t.TempDir()
	if v, err = get(); err == nil || v["5h"].Valid {
		t.Fatal("empty log root should be unknown")
	}
}

func TestCodexLatestEventNotNewestFileAndExpiredWindow(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	c := codexConfig{selection: selection{Items: []string{"primary", "secondary"}}, SessionsDir: dir}
	event := func(at, timeLimit time.Time, id string, used int) string {
		return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":%q,"primary":{"used_percent":%d,"window_minutes":300,"resets_at":%d},"secondary":{"used_percent":108,"window_minutes":10080,"resets_at":%d}}}}`+"\n", at.Format(time.RFC3339), id, used, timeLimit.Unix(), now.Add(24*time.Hour).Unix())
	}
	writeFixture(t, filepath.Join(dir, "z.jsonl"), event(now.Add(-time.Hour), now.Add(time.Hour), "", 50)+event(now, now.Add(time.Hour), "codex_spark", 91))
	writeFixture(t, filepath.Join(dir, "2026", "11", "23", "a.jsonl"), event(now.Add(-time.Minute), now.Add(time.Minute), "codex", 0))
	cache := logCache[codexEvent]{}
	v, err := fetchCodex(context.Background(), c, cache, now)
	if err != nil || !v["primary"].Valid || v["primary"].Percent != 0 || v["secondary"].Percent != 108 {
		t.Fatal(v, err)
	}
	v, err = fetchCodex(context.Background(), c, cache, now.Add(time.Minute))
	if err == nil || v["primary"].Valid || !v["secondary"].Valid {
		t.Fatal(v, err)
	}
	writeFixture(t, filepath.Join(dir, "later.jsonl"), fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"resets_at":%d}}}}`, now.Format(time.RFC3339), now.Add(time.Hour).Unix()))
	v, err = fetchCodex(context.Background(), c, cache, now)
	if err == nil || v["primary"].Valid || v["secondary"].Valid {
		t.Fatal("incomplete latest snapshot", v, err)
	}
}

func TestOpenCodeModelIdentityCacheAndRealZero(t *testing.T) {
	now := time.Unix(1000000, 0)
	c := openConfig{Models: []string{"glm", "deepseek", "@shared", "mimo"}, ModelRows: map[string]string{"mimo": "quota-providers.custom.account.mimo"}}
	raw := `{"version":3,"providers":{
	"opencode-go":{"status":"ok","entries":[
		{"id":"opencode-go.five_hour","name":"glm","window":"5h","renderType":"percent","resultType":"quota","percentRemaining":64,"resetAt":1000010},
		{"modelId":"deepseek","window":"5h","renderType":"percent","resultType":"quota","percentRemaining":0,"resetAt":1000020},
		{"modelId":"glm","window":"Weekly","renderType":"percent","resultType":"quota","percentRemaining":73,"resetAt":1000010}]},
	"quota-providers":{"status":"ok","entries":[{"id":"quota-providers.custom.account.mimo","ownership":"user_configured","window":"5h","renderType":"percent","resultType":"quota","percentRemaining":26,"resetAt":1000010}]}}}`
	v, err := parseOpenCode([]byte(raw), c, now)
	if err == nil || v["glm"].Valid || !v["deepseek"].Valid || v["deepseek"].Percent != 0 || v["@shared"].Percent != 64 || v["mimo"].Percent != 26 {
		t.Fatal(v, err)
	}
	v, _ = parseOpenCode([]byte(raw), c, now.Add(11*time.Second))
	if v["@shared"].Valid || v["mimo"].Valid || !v["deepseek"].Valid {
		t.Fatal(v)
	}
	v, err = parseOpenCode([]byte(strings.Replace(raw, `"percentRemaining":0`, `"percentRemaining":null`, 1)), c, now)
	if err == nil || v["deepseek"].Valid {
		t.Fatal("missing interpreted as zero")
	}
}

func TestConfigReloadOrderAndDisable(t *testing.T) {
	f := filepath.Join(t.TempDir(), "config.toml")
	writeFixture(t, f, "display_order=['opencode','claude','codex']\n[opencode]\nmodels=['z','a']\n[claude]\nenabled=false\n[codex]\nitems=['secondary']\n")
	c, err := readConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	ps := panels(c)
	if len(ps) != 2 || ps[0].ID != "opencode" || strings.Join(ps[0].Keys, ",") != "z,a" || ps[1].Keys[0] != "secondary" || c.Interval != 300 {
		t.Fatal(ps, c.Interval)
	}
	writeFixture(t, f, "display_order=['antigravity']\ninterval_seconds=8\n")
	c, err = readConfig(f)
	if err != nil || c.Interval != 8 || len(panels(c)) != 1 {
		t.Fatal(c, err)
	}
	writeFixture(t, f, "interval_secnds=8\n")
	if _, err := readConfig(f); err == nil {
		t.Fatal("unknown option accepted")
	}
}
