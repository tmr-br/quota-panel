package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
		if r.Header.Get("X-Codeium-Csrf-Token") == "" {
			http.Error(w, "Missing X-Codeium-Csrf-Token", http.StatusForbidden)
			return
		}
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
	// The language server advertises only the IDE callback. Its quota RPC
	// listener must be found through the PID's owned loopback sockets.
	psFixture(t, fmt.Sprintf(`case "$*" in
  *pid=,comm=*) echo '%d /Applications/Antigravity IDE.app/bin/language_server_macos_arm' ;;
  *uid=,args=*) echo '%d /Applications/Antigravity IDE.app/bin/language_server_macos_arm --extension_server_port 1 --csrf_token unit-test-token --extension_server_csrf_token=callback' ;;
  *) exit 2 ;;
esac`, os.Getpid(), os.Getuid()))
	result, err := fetchAntigravity(context.Background(), keys, now, fetchRPC)
	if err != nil || result["gemini.5h"].Percent != 83 || !result["claude_gpt.week"].Valid || result["claude_gpt.week"].Percent != 0 {
		t.Fatal(result, err)
	}
	if !slices.Equal(calls, []string{rpcPrefix + "RetrieveUserQuotaSummary", rpcPrefix + "GetUserStatus"}) {
		t.Fatal(calls)
	}
	flags := antigravityFlags(strings.Fields("Antigravity/language_server_linux --extension_server_port " + u.Port() + " --csrf_token=a --extension_server_csrf_token=b"))
	if eps := languageServerEndpoints(os.Getpid(), flags, []netip.AddrPort{netip.MustParseAddrPort(u.Host)}); len(eps) != 0 {
		t.Fatal("IDE callback was mistaken for a language server listener")
	}
	if antigravityFlags([]string{"OtherIDE/language_server", "--project=/workspace/antigravity", "--csrf_token=sample"}) != nil {
		t.Fatal("accepted a different IDE")
	}
}

func psFixture(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	ps := filepath.Join(dir, "ps")
	writeFixture(t, ps, "#!/bin/sh\n"+script+"\n")
	if err := os.Chmod(ps, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestRPCAmbiguousAccountsAreUnavailableBeforeConnecting(t *testing.T) {
	for _, order := range []string{"188 189", "189 188"} {
		t.Run(order, func(t *testing.T) {
			psFixture(t, fmt.Sprintf(`case "$*" in
  *pid=,comm=*) for p in %s; do echo "$p /Applications/Antigravity.app/bin/language_server"; done ;;
  *uid=,args=*) echo '%d /Applications/Antigravity.app/bin/language_server --https_port=1 --csrf_token=sample' ;;
  *) exit 2 ;;
esac`, order, os.Getuid()))
			v, err := fetchRPC(context.Background(), []string{"gemini.5h"}, time.Now())
			if err == nil || !strings.Contains(err.Error(), "active account") || v["gemini.5h"].Valid {
				t.Fatal(v, err)
			}
		})
	}
}

func TestDynamicRPCWithholdsTokenAtUnidentifiedListener(t *testing.T) {
	const token = "language-server-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Codeium-Csrf-Token") != "" {
			t.Error("unexpected credential")
		}
		http.Error(w, "<html>missing csrf field in login form</html>", http.StatusForbidden)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ep := localEndpoint{pid: os.Getpid(), addr: netip.MustParseAddrPort(u.Host), scheme: "http", token: token}
	v, err := queryEndpoints(context.Background(), []localEndpoint{ep}, []string{"gemini.5h"}, time.Now())
	if err == nil || v["gemini.5h"].Valid {
		t.Fatal(v, err)
	}
}

func TestListenerDiscoveryIsBoundToPIDAndLoopback(t *testing.T) {
	report := "p188\nn127.0.0.1:3090\nn[::1]:3091\nn*:4999\nn0.0.0.0:5000\nn192.0.2.1:5001\np189\nn127.0.0.1:6000"
	want := []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:3090"), netip.MustParseAddrPort("[::1]:3091")}
	if got := lsofLanguageServerListeners(report, 188); !slices.Equal(got, want) {
		t.Fatal(got)
	}
	// An advertised port absent from OS ownership must never be contacted.
	flags := map[string]string{"https_port": "6000", "http_port": "3090", "extension_server_port": "3091", "csrf_token": "local"}
	eps := languageServerEndpoints(188, flags, want)
	if len(eps) != 1 || eps[0].addr != want[0] || eps[0].scheme != "http" || !eps[0].advertised {
		t.Fatal("unexpected language server endpoint selection")
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

func TestRPCRechecksListenerOwnershipBeforeSending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached a listener owned by a different PID")
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	// The test process, not its parent process, opened the loopback listener.
	ep := localEndpoint{pid: os.Getppid(), addr: netip.MustParseAddrPort(u.Host), scheme: "http", token: "test", advertised: true}
	v, err := queryEndpoints(context.Background(), []localEndpoint{ep}, []string{"gemini.5h"}, time.Now())
	if err == nil || v["gemini.5h"].Valid {
		t.Fatal(v, err)
	}
}

func TestRunningCLIAnonymousQuota(t *testing.T) {
	now := time.Unix(42, 0)
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, present := r.Header[http.CanonicalHeaderKey("X-Codeium-Csrf-Token")]; present {
			t.Error("CLI requests must omit CSRF credentials")
		}
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case rpcPrefix + "RetrieveUserQuotaSummary":
			http.Error(w, "missing csrf token", http.StatusForbidden)
		case rpcPrefix + "GetUserStatus":
			io.WriteString(w, `{"userStatus":{"quotaSummary":{"groups":[{"buckets":[{"bucketId":"gemini-5h","remainingFraction":0.61,"resetTime":"2030-01-01T00:00:00Z"}]}]}}}`)
		default:
			t.Errorf("unexpected method %s", r.URL.Path)
		}
	}))
	defer server.Close()
	psFixture(t, fmt.Sprintf(`case "$*" in
  *pid=,comm=*) echo '%d' "$PATH/agy" ;;
  *) exit 2 ;;
esac`, os.Getpid()))
	installed := filepath.Join(os.Getenv("PATH"), "agy")
	testImage, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(testImage, installed); err != nil {
		t.Fatal(err)
	}
	// Exercise only the fallback. The synthetic CLI is already running, and
	// no command-line invocation is needed to retrieve its quota.
	keys := []string{"gemini.5h"}
	v, err := fetchRPC(context.Background(), keys, now)
	if err != nil || !v[keys[0]].Valid || v[keys[0]].Percent != 61 {
		t.Fatal(v, err)
	}
	if !slices.Equal(calls, []string{rpcPrefix + "RetrieveUserQuotaSummary", rpcPrefix + "GetUserStatus"}) {
		t.Fatalf("unexpected requests to running CLI: %v", calls)
	}

	// A different executable with the same process name must not be used.
	if err := os.Remove(installed); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", installed); err != nil {
		t.Fatal(err)
	}
	v, err = fetchRPC(context.Background(), keys, now)
	if err == nil || v[keys[0]].Valid || len(calls) != 2 {
		t.Fatal("accepted a different executable", v, err)
	}
}

func TestAntigravityFullQuotaWithUnsetReset(t *testing.T) {
	now := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, bucket string
		valid        bool
	}{
		{"full CLI", `{"id":"3p-weekly","remaining_fraction":1,"reset_time":"1970-01-01T00:00:00Z"}`, true},
		{"full RPC", `{"bucketId":"3p-5h","remainingFraction":1,"resetTime":"1970-01-01T00:00:00Z"}`, true},
		{"partial epoch", `{"id":"3p-weekly","remaining_fraction":0.5,"reset_time":"1970-01-01T00:00:00Z"}`, false},
		{"expired full", `{"id":"3p-weekly","remaining_fraction":1,"reset_time":"2026-11-25T00:00:00Z"}`, false},
		{"disabled", `{"id":"3p-weekly","disabled":true,"remaining_fraction":1,"reset_time":"1970-01-01T00:00:00Z"}`, false},
		{"missing reset", `{"id":"3p-weekly","remaining_fraction":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b agBucket
			if err := json.Unmarshal([]byte(tc.bucket), &b); err != nil {
				t.Fatal(err)
			}
			got := b.read(now)
			if got.Valid != tc.valid || (tc.valid && (got.Percent != 100 || !got.Reset.IsZero())) {
				t.Fatalf("unexpected reading: %+v", got)
			}
		})
	}
}
