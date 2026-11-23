package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAgyVersionGateAtCommandBoundary(t *testing.T) {
	for _, tc := range []struct {
		report string
		allow  bool
	}{
		{"1.1.9", false}, {"agy 1.1.10", false}, {"1.1.11-rc.1", false},
		{"new version available: 2.0.0", false}, {"1.1.10\n1.2.0", false}, {"unknown", false},
		{"1.1.11", true}, {"Antigravity CLI version: v1.2.0", true}, {"1.10.0", true},
	} {
		t.Run(tc.report, func(t *testing.T) {
			dir := t.TempDir()
			exe := filepath.Join(dir, "agy")
			trace := filepath.Join(dir, "args")
			writeFixture(t, exe, `#!/bin/sh
printf '%s\n' "$*" >> "$TRACE"
if [ "$1" = "--version" ]; then printf '%s\n' "$REPORT"; exit 0; fi
printf '%s\n' '{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"buckets":[{"id":"gemini-5h","remaining_fraction":0,"reset_time":"2026-11-24T14:00:00Z"}]}]}}}'
`)
			if err := os.Chmod(exe, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			t.Setenv("TRACE", trace)
			t.Setenv("REPORT", tc.report)
			now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
			fallbacks := 0
			v, err := fetchAntigravity(context.Background(), []string{"gemini.5h"}, now, func(context.Context, []string, time.Time) (readings, error) {
				fallbacks++
				return readings{"gemini.5h": measured(87, now.Add(time.Hour))}, nil
			})
			if err != nil || !v["gemini.5h"].Valid {
				t.Fatal(v, err)
			}
			b, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			want := "--version\n"
			if tc.allow {
				want += "-p /usage --output-format json\n"
				if fallbacks != 0 || v["gemini.5h"].Percent != 0 {
					t.Fatal(v)
				}
			} else if fallbacks != 1 || v["gemini.5h"].Percent != 87 {
				t.Fatal(v)
			}
			if string(b) != want {
				t.Fatalf("argv trace %q, want %q", b, want)
			}
		})
	}
}

func TestLocalRPCFallbackFromMatchingProcessArguments(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	keys := []string{"gemini.5h", "claude_gpt.week"}
	var calls []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if r.Method != "POST" || r.Header.Get("X-Codeium-Csrf-Token") != "unit-test-token" || r.Header.Get("Connect-Protocol-Version") != "1" {
			t.Error("invalid Connect request")
		}
		switch r.URL.Path {
		case rpcPrefix + "RetrieveUserQuotaSummary":
			io.WriteString(w, `{"response":{"groups":[{"buckets":[{"bucketId":"gemini-5h","remaining":{"case":"remainingFraction","value":0.83},"resetTime":"2026-11-24T16:00:00Z"}]}]}}`)
		case rpcPrefix + "GetUserStatus":
			io.WriteString(w, `{"userStatus":{"quotaSummary":{"groups":[{"buckets":[{"bucketId":"3p-weekly","remaining":{"remainingFraction":0},"resetTime":"2026-11-27T12:00:00Z"}]}]}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ps := filepath.Join(dir, "ps")
	writeFixture(t, ps, fmt.Sprintf(`#!/bin/sh
case "$*" in
  *pid=,comm=*) echo '188 /Applications/Antigravity IDE.app/bin/language_server_macos_arm' ;;
  *uid=,args=*) echo '%d /Applications/Antigravity IDE.app/bin/language_server_macos_arm --https_port=%s --csrf_token unit-test-token' ;;
  *) exit 2 ;;
esac
`, os.Getuid(), u.Port()))
	if err = os.Chmod(ps, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	result, err := fetchAntigravity(context.Background(), keys, now, fetchRPC)
	if err != nil || result["gemini.5h"].Percent != 83 || !result["claude_gpt.week"].Valid || result["claude_gpt.week"].Percent != 0 {
		t.Fatal(result, err)
	}
	if len(calls) != 2 {
		t.Fatal(calls)
	}
	port, _ := strconv.Atoi(u.Port())
	eps := endpointsFromArgs(strings.Fields("Antigravity/language_server_linux --extension_server_port " + u.Port() + " --csrf_token=a --extension_server_csrf_token=b"))
	if len(eps) != 2 || eps[0].port != port || eps[0].token != "b" {
		t.Fatal("incorrect extension route")
	}
	if len(endpointsFromArgs([]string{"OtherIDE/language_server", "--port=1234", "--csrf_token=sample"})) != 0 {
		t.Fatal("accepted a different IDE")
	}
}

func TestRPCUnknownWindowOrMissingFractionIsUnavailable(t *testing.T) {
	now := time.Unix(1, 0)
	for _, raw := range []string{
		`{"userStatus":{"cascadeModelConfigData":{"clientModelConfigs":[{"label":"Gemini","quotaInfo":{"remainingFraction":0.8,"resetTime":"2026-11-25T12:00:00Z"}}]}}}`,
		`{"groups":[{"buckets":[{"bucketId":"gemini-5h","resetTime":"2026-11-25T12:00:00Z"}]}]}`,
		`{"groups":[{"buckets":[{"bucketId":"gemini-argon-wtu-weekly","remainingFraction":0.4,"resetTime":"2026-11-25T12:00:00Z"}]}]}`,
	} {
		v, err := rpcReadings([]byte(raw), []string{"gemini.5h", "gemini.week"}, now)
		if err == nil || v["gemini.5h"].Valid || v["gemini.week"].Valid {
			t.Fatal(v, err)
		}
	}
}
