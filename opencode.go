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
	Account, Authority                                            string
	PercentRemaining                                              *float64
	ResetAt                                                       *int64
}

type openProvider struct {
	Status    string
	FetchedAt int64
	Entries   []openEntry
	Sources   []struct{ ID, Status string }
}

// show --json may retain a failed account's old rows in a partial provider.
// Only custom providers identify the successful definitions in the export.
func (p openProvider) confirmedCustomRow(rowID string) bool {
	for _, source := range p.Sources {
		if source.Status == "ok" && source.ID != "" && strings.HasPrefix(rowID, "quota-providers.custom."+source.ID+".") {
			return true
		}
	}
	return false
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

func (e openEntry) reading(fetchedAt int64, now time.Time) (reading, error) {
	if e.PercentRemaining == nil || *e.PercentRemaining > 100 {
		return reading{}, errors.New("invalid remaining percentage")
	}
	// V3 permits a negative remaining percentage when a quota is overdrawn.
	r := reading{Valid: true, Percent: *e.PercentRemaining}
	switch e.Authority {
	case "locally_derived":
		r.Estimated = true
	case "provider_reported":
	default:
		return reading{}, errors.New("unknown quota authority")
	}
	if e.ResetAt != nil {
		r.Reset = time.Unix(*e.ResetAt, 0)
	} else {
		if fetchedAt <= 0 || fetchedAt > now.Unix() {
			return reading{}, errors.New("cache freshness unknown")
		}
		r.ValidUntil = time.Unix(fetchedAt, 0).Add(5 * time.Hour)
	}
	if !r.expiresAt().After(now) {
		return reading{}, errors.New("expired cached quota")
	}
	return r, nil
}

func parseOpenCode(raw []byte, c openConfig, now time.Time) (readings, error) {
	var data struct {
		Version   int
		Providers map[string]openProvider
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
			if !p.confirmedCustomRow(rowID) {
				continue
			}
		}
		var entries []openEntry
		for _, e := range p.Entries {
			if rowID == "" && c.Account != "" && e.Account != c.Account {
				continue
			}
			if e.matches(model, rowID) {
				entries = append(entries, e)
			}
		}
		if len(entries) != 1 {
			problems = append(problems, fmt.Errorf("%s: missing or ambiguous 5h quota", model))
			continue
		}
		r, err := entries[0].reading(p.FetchedAt, now)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", model, err))
			continue
		}
		out[model] = r
	}
	return out, errors.Join(problems...)
}
