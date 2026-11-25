package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// OpenCode keeps its OpenCode Console sign-in (`opencode auth login`) in its
// credential database. The Console reports the Go plan meters for that
// member, the same source CodexBar uses. The token is only sent to the
// sign-in's own Console server and is never logged, stored or refreshed here.
const openConsoleDefault = "https://opencode.ai/console"

type openCredential struct {
	Access   string `json:"access"`
	Expires  *int64 `json:"expires"` // Unix milliseconds
	Metadata struct {
		OrgID  string `json:"orgID"`
		Server string `json:"server"`
	} `json:"metadata"`
}

type openMeter struct {
	ResetsAt *string     `json:"resetsAt"`
	Limit    json.Number `json:"limitMicroCents"`
	Used     json.Number `json:"usedMicroCents"`
}

type openGoStatus struct {
	Access *struct {
		EndsAt *string               `json:"endsAt"`
		Meters map[string]*openMeter `json:"meters"`
	} `json:"access"`
}

var openMeterKeys = map[string]string{"5h": "fiveHour", "week": "week", "month": "month"}

// Tests replace this to avoid reading the real OpenCode database.
var readOpenCredential = loadOpenCredential

func openDatabase() (string, error) {
	if p := os.Getenv("OPENCODE_DB"); p != "" {
		return p, nil
	}
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "opencode", "opencode.db"), nil
}

func loadOpenCredential(ctx context.Context) ([]byte, error) {
	db, err := openDatabase()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(db); err != nil {
		return nil, errors.New("no OpenCode credential database; run `opencode auth login`")
	}
	// The build is CGO-free, so read the SQLite file through the sqlite3 CLI.
	b, err := commandOutput(ctx, 15*time.Second, "sqlite3", "-readonly", db,
		"SELECT value FROM credential WHERE integration_id = 'opencode' AND active = 1 LIMIT 1")
	if err != nil {
		return nil, fmt.Errorf("read OpenCode credentials: %w", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, errors.New("no OpenCode Console sign-in; run `opencode auth login` and choose OpenCode")
	}
	return b, nil
}

func fetchOpenCode(ctx context.Context, c openConfig, now time.Time) (readings, error) {
	raw, err := readOpenCredential(ctx)
	if err != nil {
		return nil, err
	}
	var cred openCredential
	if err := json.Unmarshal(raw, &cred); err != nil || cred.Access == "" {
		return nil, errors.New("OpenCode Console sign-in is not an OAuth login; run `opencode auth login` and choose OpenCode")
	}
	if cred.Expires != nil && !now.Before(time.UnixMilli(*cred.Expires)) {
		return nil, errors.New("OpenCode Console sign-in expired; start `opencode` once to refresh it")
	}
	server := strings.TrimRight(cred.Metadata.Server, "/")
	if server == "" {
		server = openConsoleDefault
	}
	if !strings.HasPrefix(server, "https://") {
		return nil, fmt.Errorf("refusing non-HTTPS OpenCode Console server %q", server)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/api/go/status", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.Access)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "quota-panel/"+strings.Fields(version())[0])
	if cred.Metadata.OrgID != "" {
		req.Header.Set("x-org-id", cred.Metadata.OrgID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OpenCode Console: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, errors.New("OpenCode Console rejected the sign-in (401); start `opencode` once to refresh it")
	case http.StatusNotFound:
		return nil, errors.New("no OpenCode Go subscription for this OpenCode Console account")
	default:
		return nil, fmt.Errorf("OpenCode Console: HTTP %d", resp.StatusCode)
	}
	var status openGoStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&status); err != nil {
		return nil, fmt.Errorf("OpenCode Console: %w", err)
	}
	return parseOpenGoStatus(status, c, now)
}

func parseOpenGoStatus(s openGoStatus, c openConfig, now time.Time) (readings, error) {
	if s.Access == nil {
		return nil, errors.New("OpenCode Go subscription is not active")
	}
	out := readings{}
	var problems []error
	for _, key := range c.Items {
		m := s.Access.Meters[openMeterKeys[key]]
		if m == nil {
			problems = append(problems, fmt.Errorf("%s: meter not reported", key))
			continue
		}
		limit, err1 := strconv.ParseFloat(string(m.Limit), 64)
		used, err2 := strconv.ParseFloat(string(m.Used), 64)
		if err1 != nil || err2 != nil || limit < 0 || used < 0 {
			problems = append(problems, fmt.Errorf("%s: invalid meter amounts", key))
			continue
		}
		r := reading{Valid: true, Percent: 100}
		if limit > 0 {
			// Remaining may go negative when the plan is overdrawn.
			r.Percent = 100 - 100*used/limit
		} else if used > 0 {
			r.Percent = 0
		}
		reset := m.ResetsAt
		if (reset == nil || *reset == "") && key == "month" {
			reset = s.Access.EndsAt
		}
		if reset != nil && *reset != "" {
			t, err := time.Parse(time.RFC3339Nano, *reset)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s: invalid reset time %q", key, *reset))
				continue
			}
			if !t.After(now) {
				problems = append(problems, fmt.Errorf("%s: expired meter", key))
				continue
			}
			r.Reset = t
		} else if used > 0 {
			// A running window always has a reset. Without one the reading
			// is only trusted until the next poll would replace it.
			r.ValidUntil = now.Add(5 * time.Hour)
		}
		out[key] = r
	}
	return out, errors.Join(problems...)
}
