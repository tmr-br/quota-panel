package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeAccountSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel.toml")
	writeFixture(t, path, "[opencode]\naccount='work'\nmodels=['@shared']\n")
	c, err := readConfig(path)
	if err != nil || c.OpenCode.Account != "work" {
		t.Fatal(c.OpenCode.Account, err)
	}
	now := time.Unix(200000, 0)
	work := `{"id":"opencode-go.five_hour","account":"work","window":"5h","renderType":"percent","resultType":"quota","authority":"provider_reported","percentRemaining":90,"resetAt":200100}`
	home := `{"id":"opencode-go.five_hour","account":"home","window":"5h","renderType":"percent","resultType":"quota","authority":"provider_reported","percentRemaining":10,"resetAt":200100}`
	for _, tc := range []struct {
		name, account, status, entries string
		want                           float64 // -1 means unavailable.
	}{
		{"single", "", "ok", home, 10},
		{"ambiguous", "", "ok", work + "," + home, -1},
		{"missing standard authority", "", "ok", strings.Replace(work, `"authority":"provider_reported",`, "", 1), -1},
		{"unknown standard authority", "", "ok", strings.Replace(work, "provider_reported", "other", 1), -1},
		{"selected", c.OpenCode.Account, "ok", home + "," + work, 90},
		{"absent", "home", "ok", work, -1},
		{"duplicate selected", "work", "ok", work + "," + work, -1},
		{"failed account unidentifiable", "", "partial", work, -1},
		{"failed account missing", "home", "partial", work, -1},
		{"failed account retained in cache", "home", "partial", home + "," + work, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := c.OpenCode
			config.Account = tc.account
			raw := fmt.Sprintf(`{"version":3,"providers":{"opencode-go":{"status":%q,"fetchedAt":200000,"entries":[%s]}}}`, tc.status, tc.entries)
			values, err := parseOpenCode([]byte(raw), config, now)
			v := values["@shared"]
			if tc.want < 0 {
				if err == nil || v.Valid {
					t.Fatalf("got %+v, %v", v, err)
				}
			} else if err != nil || !v.Valid || v.Percent != tc.want || v.Estimated {
				t.Fatalf("got %+v, %v", v, err)
			}
		})
	}
}

func TestOpenCodeCustomProvenanceAndOverage(t *testing.T) {
	now := time.Unix(200000, 0)
	config := openConfig{Models: []string{"m"}, ModelRows: map[string]string{"m": "quota-providers.custom.vendor.m"}, Account: "elsewhere"}
	for _, tc := range []struct {
		name, authority, remaining string
		valid, estimated           bool
		want                       float64
	}{
		{"provider overage", "provider_reported", "-5", true, false, -5},
		{"local estimate", "locally_derived", "100", true, true, 100},
		{"exhausted", "provider_reported", "0", true, false, 0},
		{"no authority", "", "50", false, false, 0},
		{"unknown authority", "unknown", "50", false, false, 0},
		{"missing quota", "provider_reported", "null", false, false, 0},
		{"outside range", "provider_reported", "101", false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"version":3,"providers":{"quota-providers":{"status":"ok","entries":[{"id":"quota-providers.custom.vendor.m","ownership":"user_configured","authority":%q,"renderType":"percent","resultType":"quota","window":"5h","percentRemaining":%s,"resetAt":200100}]}}}`, tc.authority, tc.remaining)
			values, err := parseOpenCode([]byte(raw), config, now)
			v := values["m"]
			if (err == nil) != tc.valid || v.Valid != tc.valid || v.Estimated != tc.estimated || v.Percent != tc.want {
				t.Fatalf("got %+v, %v", v, err)
			}
		})
	}
}

func TestOpenCodePartialCustomSource(t *testing.T) {
	now := time.Unix(200000, 0)
	config := openConfig{Models: []string{"m"}, ModelRows: map[string]string{"m": "quota-providers.custom.vendor.m"}}
	const report = `{"version":3,"providers":{"quota-providers":{"status":"partial","sources":%s,"entries":[{"id":"quota-providers.custom.vendor.m","ownership":"user_configured","authority":"provider_reported","renderType":"percent","resultType":"quota","window":"5h","percentRemaining":37,"resetAt":200100}]}}}`
	for _, tc := range []struct {
		sources string
		valid   bool
	}{
		{`[{"id":"other","status":"error"},{"id":"vendor","status":"ok"}]`, true},
		{`[{"id":"vendor","status":"error"},{"id":"other","status":"ok"}]`, false},
		{`[]`, false},
	} {
		values, err := parseOpenCode([]byte(fmt.Sprintf(report, tc.sources)), config, now)
		if got := values["m"]; err == nil || got.Valid != tc.valid {
			t.Fatalf("sources %s: %+v, %v", tc.sources, got, err)
		}
	}
}

func TestOpenCodeCacheExpiryIsIndependentFromReset(t *testing.T) {
	now := time.Unix(200000, 0)
	fetchedAt := now.Add(-4*time.Hour - 59*time.Minute)
	raw := fmt.Sprintf(`{"version":3,"providers":{"opencode-go":{"status":"ok","fetchedAt":%d,"entries":[{"id":"opencode-go.five_hour","window":"5h","renderType":"percent","resultType":"quota","authority":"provider_reported","percentRemaining":67}]}}}`, fetchedAt.Unix())
	config := openConfig{Models: []string{"@shared"}}
	values, err := parseOpenCode([]byte(raw), config, now)
	if err != nil {
		t.Fatal(err)
	}
	p := panel{Keys: config.Models, Values: values}
	deadline := now.Add(time.Minute)
	v := p.valueAt("@shared", deadline.Add(-time.Nanosecond))
	if !v.Valid || !v.Reset.IsZero() || !v.ValidUntil.Equal(deadline) || !v.expiresAt().Equal(deadline) {
		t.Fatalf("before cache expiry: %+v", v)
	}
	if next := nextExpiration([]panel{p}, now); !next.Equal(deadline) {
		t.Fatalf("next display update %v, want cache expiry %v", next, deadline)
	}
	if p.valueAt("@shared", deadline).Valid {
		t.Fatal("cache valid at expiry")
	}
	if values, err := parseOpenCode([]byte(raw), config, deadline); err == nil || values["@shared"].Valid {
		t.Fatal("expired cache accepted again", values)
	}
}
