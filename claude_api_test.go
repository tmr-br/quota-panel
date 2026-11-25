package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func stubClaudeAPI(t *testing.T, cred string, status int, body string) *int {
	t.Helper()
	calls := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" || !strings.HasPrefix(r.Header.Get("User-Agent"), "quota-panel/") {
			t.Errorf("unexpected headers %v", r.Header)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	oldRead, oldURL := readClaudeCredential, claudeUsageEndpoint
	t.Cleanup(func() { readClaudeCredential, claudeUsageEndpoint = oldRead, oldURL })
	readClaudeCredential = func(context.Context) ([]byte, error) { return []byte(cred), nil }
	claudeUsageEndpoint = srv.URL
	return calls
}

func claudeCred(expires time.Time, scopes string) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok","refreshToken":"r","expiresAt":%d,"scopes":[%s]}}`, expires.UnixMilli(), scopes)
}

func TestClaudeUsageAPIReportsAccountPercentages(t *testing.T) {
	now := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	stubClaudeAPI(t, claudeCred(now.Add(time.Hour), `"user:profile"`), 200,
		`{"five_hour":{"utilization":29.0,"resets_at":"2026-11-25T10:00:00.380999+00:00"},"seven_day":{"utilization":19,"resets_at":"2026-11-29T10:00:00+00:00"},"seven_day_opus":null}`)
	c := claudeConfig{selection: selection{Items: []string{"5h", "week"}}, UsageAPI: true}
	v, err := fetchClaude(context.Background(), c, logCache[claudeEvent]{}, now)
	if err != nil || v["5h"].Percent != 29 || v["week"].Percent != 19 || v["5h"].Estimated || !v["week"].Reset.Equal(time.Date(2026, 11, 29, 10, 0, 0, 0, time.UTC)) {
		t.Fatal(v, err)
	}
}

func TestClaudeUsageAPIFailures(t *testing.T) {
	now := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	c := claudeConfig{selection: selection{Items: []string{"5h", "week"}}, UsageAPI: true}
	for name, tc := range map[string]struct {
		cred   string
		status int
		body   string
		want   string
		called bool
	}{
		"expired sign-in": {claudeCred(now.Add(-time.Minute), `"user:profile"`), 200, `{}`, "expired", false},
		"missing scope":   {claudeCred(now.Add(time.Hour), `"user:inference"`), 200, `{}`, "scope", false},
		"rejected":        {claudeCred(now.Add(time.Hour), `"user:profile"`), 401, `{}`, "401", true},
		"null window":     {claudeCred(now.Add(time.Hour), `"user:profile"`), 200, `{"five_hour":{"utilization":3,"resets_at":"2026-11-25T10:00:00Z"},"seven_day":null}`, "week: not reported", true},
	} {
		t.Run(name, func(t *testing.T) {
			calls := stubClaudeAPI(t, tc.cred, tc.status, tc.body)
			_, err := fetchClaude(context.Background(), c, logCache[claudeEvent]{}, now)
			if err == nil || !strings.Contains(err.Error(), tc.want) || (*calls > 0) != tc.called {
				t.Fatal(err, *calls)
			}
			if strings.Contains(err.Error(), "tok") {
				t.Fatal("error leaks the token:", err)
			}
		})
	}
}

func TestClaudeUsageAPIFallsBackToConfiguredEstimate(t *testing.T) {
	now := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	stubClaudeAPI(t, claudeCred(now.Add(time.Hour), `"user:profile"`), 500, `{}`)
	c := claudeConfig{selection: selection{Items: []string{"5h"}}, UsageAPI: true, ProjectsDir: t.TempDir(), FiveHourTokens: 1000}
	_, err := fetchClaude(context.Background(), c, logCache[claudeEvent]{}, now)
	if err == nil || !strings.Contains(err.Error(), "using the local estimate") || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatal(err)
	}
}
