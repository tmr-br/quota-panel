package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Tests never read the real Keychain or OpenCode database, so daemon tests
// that load the default configuration stay offline.
func init() {
	readOpenCredential = func(context.Context) ([]byte, error) { return nil, errors.New("test: no OpenCode sign-in") }
	readClaudeCredential = func(context.Context) ([]byte, error) { return nil, errors.New("test: no Claude sign-in") }
}

func openStatus(t *testing.T, raw string) openGoStatus {
	t.Helper()
	var s openGoStatus
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOpenCodeGoRejectsNonNumericAmounts(t *testing.T) {
	var s openGoStatus
	if err := json.Unmarshal([]byte(`{"access":{"meters":{"fiveHour":{"limitMicroCents":"x","usedMicroCents":"0"}}}}`), &s); err == nil {
		t.Fatal("accepted a non-numeric amount")
	}
}

func TestOpenCodeGoMetersBecomeRemainingPercentages(t *testing.T) {
	now := time.Date(2026, 11, 25, 7, 0, 0, 0, time.UTC)
	s := openStatus(t, `{"access":{"endsAt":"2026-12-06T08:59:41.000Z","meters":{
		"fiveHour":{"startsAt":null,"resetsAt":null,"limitMicroCents":"1200000000","usedMicroCents":"0"},
		"week":{"resetsAt":"2026-11-30T00:00:00.000Z","limitMicroCents":"3000000000","usedMicroCents":"3300000000"},
		"month":{"limitMicroCents":6000000000,"usedMicroCents":"1500000000"}}}}`)
	v, err := parseOpenGoStatus(s, openConfig{selection{Items: []string{"5h", "week", "month"}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	// An unstarted window is fully available; an overdrawn one is negative;
	// a month without its own reset uses the plan's end.
	if !v["5h"].Valid || v["5h"].Percent != 100 || !v["5h"].Reset.IsZero() ||
		v["week"].Percent > -9.99 || v["week"].Percent < -10.01 ||
		v["month"].Percent != 75 || !v["month"].Reset.Equal(time.Date(2026, 12, 6, 8, 59, 41, 0, time.UTC)) {
		t.Fatal(v)
	}
}

func TestOpenCodeGoRejectsUnusableMeters(t *testing.T) {
	now := time.Date(2026, 11, 25, 7, 0, 0, 0, time.UTC)
	c := openConfig{selection{Items: []string{"5h"}}}
	for name, raw := range map[string]string{
		"not subscribed": `{"access":null}`,
		"missing meter":  `{"access":{"meters":{}}}`,
		"negative":       `{"access":{"meters":{"fiveHour":{"limitMicroCents":"10","usedMicroCents":"-1"}}}}`,
		"expired":        `{"access":{"meters":{"fiveHour":{"resetsAt":"2026-11-25T06:00:00Z","limitMicroCents":"10","usedMicroCents":"1"}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			v, err := parseOpenGoStatus(openStatus(t, raw), c, now)
			if err == nil || v["5h"].Valid {
				t.Fatal(v, err)
			}
		})
	}
}

func TestOpenCodeConsoleRequest(t *testing.T) {
	now := time.Date(2026, 11, 25, 7, 0, 0, 0, time.UTC)
	var status int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/console/api/go/status" || r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("x-org-id") != "org_1" {
			t.Errorf("unexpected request %s %v", r.URL, r.Header)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, `{"access":{"meters":{"week":{"resetsAt":"2026-11-30T00:00:00Z","limitMicroCents":"100","usedMicroCents":"25"}}}}`)
	}))
	defer srv.Close()
	oldClient := http.DefaultClient
	http.DefaultClient = srv.Client()
	defer func() { http.DefaultClient = oldClient }()
	old := readOpenCredential
	defer func() { readOpenCredential = old }()
	cred := func(expires time.Time) {
		readOpenCredential = func(context.Context) ([]byte, error) {
			return []byte(fmt.Sprintf(`{"type":"oauth","access":"tok","refresh":"r","expires":%d,"metadata":{"orgID":"org_1","server":%q}}`, expires.UnixMilli(), srv.URL+"/console")), nil
		}
	}
	c := openConfig{selection{Items: []string{"week"}}}

	cred(now.Add(time.Hour))
	status = 200
	if v, err := fetchOpenCode(context.Background(), c, now); err != nil || v["week"].Percent != 75 {
		t.Fatal(v, err)
	}
	for code, want := range map[int]string{401: "401", 404: "subscription", 500: "HTTP 500"} {
		status = code
		if _, err := fetchOpenCode(context.Background(), c, now); err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "tok") {
			t.Fatal(code, err)
		}
	}
	cred(now.Add(-time.Minute))
	if _, err := fetchOpenCode(context.Background(), c, now); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatal(err)
	}
}
