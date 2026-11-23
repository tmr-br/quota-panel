package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func fetchOpenCode(ctx context.Context, c openConfig, now time.Time) (readings, error) {
	args := []string{"--yes", "@slkiser/opencode-quota@latest", "show", "--json"}
	if len(c.ModelRows) == 0 {
		args = append(args, "--provider", "opencode-go")
	}
	b, err := commandOutput(ctx, 45*time.Second, "npx", args...)
	if err != nil {
		return nil, err
	}
	return parseOpenCode(b, c, now)
}

type openEntry struct {
	ID, Window, RenderType, ResultType, Model, ModelID, Ownership string
	PercentRemaining                                              *float64
	ResetAt                                                       *int64
}

func (e openEntry) matches(model, rowID string) bool {
	if !strings.EqualFold(e.Window, "5h") || e.RenderType != "percent" || e.ResultType != "quota" {
		return false
	}
	if rowID != "" {
		return e.ID == rowID && e.Ownership == "user_configured"
	}
	name := e.ModelID
	if name == "" {
		name = e.Model
	}
	if name != "" {
		return name == model && model != "@shared"
	}
	if model == "@shared" {
		return e.ID == "opencode-go.five_hour"
	}
	// Only an explicitly model-scoped id identifies a model. A customizable
	// display name or an account name is not evidence of a model quota.
	return e.ID == "opencode-go."+model+".five_hour" || e.ID == "opencode-go.five_hour."+model
}

func parseOpenCode(raw []byte, c openConfig, now time.Time) (readings, error) {
	var data struct {
		Version   int
		Providers map[string]struct {
			Status    string
			FetchedAt int64
			Entries   []openEntry
		}
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if data.Version != 3 {
		return nil, fmt.Errorf("unsupported opencode-quota JSON version %d", data.Version)
	}
	out := readings{}
	var problems []error
	for _, model := range c.Models {
		provider := "opencode-go"
		rowID := c.ModelRows[model]
		if rowID != "" {
			provider = "quota-providers"
		}
		p, ok := data.Providers[provider]
		if !ok || (p.Status != "ok" && p.Status != "partial") {
			problems = append(problems, fmt.Errorf("%s: %s quota cache unavailable", model, provider))
			continue
		}
		if p.Status == "partial" {
			problems = append(problems, fmt.Errorf("%s: %s reports partial data", model, provider))
		}
		var entries []openEntry
		for _, e := range p.Entries {
			if e.matches(model, rowID) {
				entries = append(entries, e)
			}
		}
		if len(entries) != 1 || entries[0].PercentRemaining == nil {
			problems = append(problems, fmt.Errorf("%s: missing or ambiguous 5h quota", model))
			continue
		}
		e := entries[0]
		var reset time.Time
		if e.ResetAt != nil {
			reset = time.Unix(*e.ResetAt, 0)
			if !reset.After(now) {
				problems = append(problems, fmt.Errorf("%s: expired cached quota", model))
				continue
			}
		} else if p.FetchedAt <= 0 || p.FetchedAt > now.Unix() || !time.Unix(p.FetchedAt, 0).Add(5*time.Hour).After(now) {
			problems = append(problems, fmt.Errorf("%s: cache freshness unknown", model))
			continue
		}
		r := measured(*e.PercentRemaining, reset)
		if !r.Valid {
			problems = append(problems, fmt.Errorf("%s: invalid remaining percentage", model))
			continue
		}
		out[model] = r
	}
	return out, errors.Join(problems...)
}
