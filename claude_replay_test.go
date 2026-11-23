package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Mirrors Claude Code's agent_progress wrapper and its copy in subagents/.
func TestClaudeProgressAndAdvisorUsage(t *testing.T) {
	const inner = `{"type":"assistant","timestamp":"2026-01-02T10:00:00Z","uuid":"node","requestId":"req-a","message":{"id":"msg-a","model":"sonnet","usage":{"input_tokens":100,"output_tokens":1,"iterations":[{"type":"message","input_tokens":100,"output_tokens":1},{"type":"advisor_message","model":"opus","input_tokens":10,"output_tokens":2}]}}}`
	const wrapped = `{"type":"progress","sessionId":"session","data":{"type":"agent_progress","message":` + inner + `}}`
	const ordinary = `{"type":"assistant","timestamp":"2026-01-02T10:05:00Z","requestId":"req-b","message":{"id":"msg-b","usage":{"input_tokens":10,"output_tokens":5}}}`
	const progressText = `{"type":"progress","data":{"type":"mcp_progress","message":"Connecting"}}`
	dir := t.TempDir()
	cfg := claudeConfig{ProjectsDir: dir, selection: selection{Items: []string{"5h", "week"}}, FiveHourTokens: 1000, WeeklyTokens: 1000, WeeklyResetAnchor: "2026-01-04T00:00:00Z"}
	now := time.Date(2026, 1, 2, 11, 0, 0, 0, time.UTC)
	cache := logCache[claudeEvent]{}
	file := filepath.Join(dir, "session.jsonl")
	writeFixture(t, file, strings.Join([]string{ordinary, wrapped, wrapped, progressText}, "\n"))
	check := func(want float64) {
		t.Helper()
		v, err := fetchClaude(context.Background(), cfg, cache, now)
		for _, k := range cfg.Items {
			if err != nil || !v[k].Valid || v[k].Percent != want {
				t.Fatalf("%s = %+v, error %v; want %v%%", k, v[k], err, want)
			}
		}
	}
	check(12.8)
	// A direct copy can have a fuller main total but omit advisor details.
	direct := strings.Replace(inner, `"output_tokens":1,"iterations"`, `"output_tokens":11,"ignored"`, 1)
	writeFixture(t, filepath.Join(dir, "subagents", "agent.jsonl"), direct)
	check(13.8)
	writeFixture(t, file, strings.Replace(wrapped, `"output_tokens":2}`, `"output_tokens":null}`, 1))
	v, err := fetchClaude(context.Background(), cfg, cache, now)
	if err == nil || v["5h"].Valid || v["week"].Valid {
		t.Fatalf("incomplete advisor: %v %v", v, err)
	}
}

func TestClaudeRequestIdentity(t *testing.T) {
	now := time.Date(2026, 1, 2, 11, 0, 0, 0, time.UTC)
	first := `{"type":"assistant","uuid":"a","sessionId":"s","timestamp":"2026-01-02T10:00:00Z","message":{"id":"gateway","model":"sonnet","content":[{"type":"tool_use","id":"tool-1"}],"usage":{"input_tokens":100,"output_tokens":25,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}}`
	last := `{"type":"assistant","uuid":"b","parentUuid":"a","sessionId":"s","timestamp":"2026-01-02T10:00:01Z","message":{"id":"gateway","model":"sonnet","usage":{"input_tokens":100,"output_tokens":250,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}}`
	through := strings.Replace(last, `"parentUuid":"a"`, `"parentUuid":"middle"`, 1)
	answer := `{"type":"user","uuid":"middle","parentUuid":"a","sessionId":"s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1"}]}}`
	main := strings.Replace(first, `"uuid":"a"`, `"uuid":"a","requestId":"primary"`, 1)
	replay := strings.Replace(last, `"uuid":"b"`, `"uuid":"b","requestId":"copy","isSidechain":true`, 1)
	for _, tc := range []struct {
		name  string
		lines []string
		want  int64
	}{
		{"same response at two timestamps", []string{first, last}, 365},
		{"requestless unrelated gateway response", []string{first, strings.Replace(last, `"parentUuid":"a"`, `"parentUuid":"other"`, 1)}, 505},
		{"new input starts a response", []string{first, strings.Replace(last, `"input_tokens":100`, `"input_tokens":120`, 1)}, 525},
		{"owned tool result carries the chain", []string{first, answer, through}, 365},
		{"unrelated result separates responses", []string{first, strings.Replace(answer, "tool-1", "another-tool", 1), through}, 505},
		{"sidechain copy with new request ID", []string{main, replay}, 140},
		{"sidechain before original", []string{replay, main}, 140},
		{"same message different ordinary requests", []string{main, strings.Replace(replay, `"isSidechain":true`, `"isSidechain":false`, 1)}, 505},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []claudeEvent
			for _, line := range tc.lines {
				e, ok, err := parseClaude("s", []byte(line))
				if err != nil {
					t.Fatal(err)
				}
				if ok {
					events = append(events, e)
				}
			}
			out, err := uniqueClaude(events, now)
			if err != nil {
				t.Fatal(err)
			}
			var total int64
			for _, r := range out {
				total += r.tokens
			}
			if total != tc.want {
				t.Fatalf("got %d tokens, want %d", total, tc.want)
			}
		})
	}
}

func TestClaudeMissingSessionIDUsesTranscriptPath(t *testing.T) {
	dir := t.TempDir()
	cfg := claudeConfig{ProjectsDir: dir, selection: selection{Items: []string{"5h"}}, FiveHourTokens: 100}
	for i, n := range []int{10, 20} {
		raw := fmt.Sprintf(`{"type":"assistant","timestamp":"2026-01-02T10:00:00Z","message":{"id":"gateway","model":"sonnet","usage":{"input_tokens":%d,"output_tokens":1}}}`, n)
		writeFixture(t, filepath.Join(dir, fmt.Sprintf("%d.jsonl", i)), raw)
	}
	v, err := fetchClaude(context.Background(), cfg, logCache[claudeEvent]{}, time.Date(2026, 1, 2, 11, 0, 0, 0, time.UTC))
	if err != nil || v["5h"].Percent != 32 {
		t.Fatal(v, err)
	}
}

func TestClaudeAmbiguousSidechainAndMissingMainUsage(t *testing.T) {
	now := time.Date(2026, 1, 2, 11, 0, 0, 0, time.UTC)
	base := claudeEvent{session: "s", message: "reused-id", request: "req-1", at: now, amounts: []int64{10}}
	second := base
	second.request = "req-2"
	replay := base
	replay.request, replay.sidechain = "req-3", true
	_, err := uniqueClaude([]claudeEvent{base, second, replay}, now)
	if err == nil {
		t.Fatal("a replay with two unrelated candidate requests was attributed silently")
	}
	_, _, err = parseClaude("s", []byte(`{"type":"assistant","timestamp":"2026-01-02T10:00:00Z","message":{"id":"msg","model":"sonnet"}}`))
	if err == nil {
		t.Fatal("missing real-model usage silently counted as no usage")
	}
}

func TestClaudeRequestlessRewriteOfSidechainReplay(t *testing.T) {
	first := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	now := first.Add(time.Hour)
	alias := claudeEvent{at: first, session: "s", message: "msg_parent", amounts: []int64{125}}
	replay := alias
	replay.sidechain = true
	parent := alias
	parent.at, parent.request = first.Add(time.Second), "req_parent"
	// The requestless ordinary record and replay share an exact rewrite key.
	// File enumeration order must not decide whether the parent absorbs them.
	for _, events := range [][]claudeEvent{
		{replay, parent, alias}, {replay, alias, parent}, {alias, replay, parent},
		{alias, parent, replay}, {parent, alias, replay}, {parent, replay, alias},
	} {
		out, err := uniqueClaude(events, now)
		if err != nil || len(out) != 1 || out[0].tokens != 125 {
			t.Fatalf("expected one 125-token response, got %v (%v)", out, err)
		}
	}
	// The same message ID in two ordinary gateway requests does not identify
	// which one owns the replay, even when its requestless rewrite is present.
	other := parent
	other.request = "req_other"
	if _, err := uniqueClaude([]claudeEvent{replay, parent, alias, other}, now); err == nil {
		t.Fatal("accepted two possible explicit parents for a requestless replay")
	}
	// A requestless ordinary response at another timestamp cannot claim the
	// alias relationship just because its message ID was reused.
	unrelated := alias
	unrelated.at = first.Add(-time.Second)
	if _, err := uniqueClaude([]claudeEvent{unrelated, replay, parent}, now); err == nil {
		t.Fatal("accepted an unrelated ordinary response as a rewrite of the replay")
	}
}
