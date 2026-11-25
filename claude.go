package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// A transcript record keeps quota counters and the IDs needed to identify
// replays. User text, tool inputs and tool outputs are never retained.
type claudeEvent struct {
	at                                      time.Time
	session, message, request, uuid, parent string
	sidechain                               bool
	inputs                                  [3]int64 // input, effective cache creation, cache read
	amounts                                 []int64  // main response followed by its separate advisor usages
	tools                                   []string // tool-use IDs or, for a link, answered tool-use IDs
}

type tokenUsage struct {
	Input         *int64 `json:"input_tokens"`
	Output        *int64 `json:"output_tokens"`
	Creation      int64  `json:"cache_creation_input_tokens"`
	Read          int64  `json:"cache_read_input_tokens"`
	CacheCreation *struct {
		FiveMinutes int64 `json:"ephemeral_5m_input_tokens"`
		OneHour     int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (u tokenUsage) counts() ([3]int64, int64, error) {
	inputs := [3]int64{}
	if u.Input == nil || u.Output == nil {
		return inputs, 0, errors.New("incomplete token usage")
	}
	if u.Creation < 0 {
		return inputs, 0, errors.New("invalid cache creation token count")
	}
	creation := u.Creation
	if split := u.CacheCreation; split != nil {
		if split.FiveMinutes < 0 || split.OneHour < 0 || split.FiveMinutes > math.MaxInt64-split.OneHour {
			return inputs, 0, errors.New("invalid cache creation token count")
		}
		creation = max(creation, split.FiveMinutes+split.OneHour)
	}
	inputs = [3]int64{*u.Input, creation, u.Read}
	total := *u.Output
	if total < 0 {
		return inputs, 0, errors.New("invalid output token count")
	}
	for _, n := range inputs {
		if n < 0 || n > math.MaxInt64-total {
			return inputs, 0, errors.New("invalid token count")
		}
		total += n
	}
	return inputs, total, nil
}

type claudeEnvelope struct {
	Type, Timestamp, UUID, ParentUUID, SessionID, RequestID string
	IsSidechain                                             bool
	Message, Data                                           json.RawMessage
}

func parseClaude(path string, line []byte) (claudeEvent, bool, error) {
	var row claudeEnvelope
	var e claudeEvent
	if err := json.Unmarshal(line, &row); err != nil {
		return e, false, err
	}
	if row.Type == "progress" || (row.Type == "" && len(row.Data) > 0) {
		var data struct {
			Type    string
			Message json.RawMessage
		}
		if len(row.Data) == 0 || row.Data[0] != '{' {
			return e, false, nil
		}
		if err := json.Unmarshal(row.Data, &data); err != nil {
			return e, false, err
		}
		if (data.Type != "" && data.Type != "agent_progress") || len(data.Message) == 0 || data.Message[0] != '{' {
			return e, false, nil
		}
		var nested claudeEnvelope
		if err := json.Unmarshal(data.Message, &nested); err != nil {
			return e, false, err
		}
		if nested.SessionID == "" {
			nested.SessionID = row.SessionID
		}
		nested.IsSidechain = nested.IsSidechain || row.IsSidechain
		row = nested
	}
	e.session = row.SessionID
	if e.session == "" {
		e.session = path
	}
	e.uuid, e.parent, e.sidechain = row.UUID, row.ParentUUID, row.IsSidechain
	link := e.uuid != "" && e.parent != ""
	if row.Type != "" && row.Type != "assistant" && row.Type != "user" {
		return e, link, nil
	}
	if len(row.Message) == 0 {
		if row.Type == "assistant" {
			return e, false, errors.New("assistant record missing message")
		}
		return e, false, nil
	}
	var message struct {
		ID, Model, Role string
		Content         json.RawMessage
		Usage           *struct {
			tokenUsage
			Iterations []struct {
				Type string
				tokenUsage
			}
		}
	}
	if err := json.Unmarshal(row.Message, &message); err != nil {
		return e, false, err
	}
	if row.Type == "user" || message.Role == "user" {
		if !link {
			return e, false, nil
		}
		// Only results belonging to this same response may bridge its
		// content blocks. An actual user prompt starts a new response.
		var err error
		e.tools, err = claudeToolIDs(message.Content, true)
		return e, len(e.tools) > 0, err
	}
	if message.Model == "<synthetic>" {
		return e, false, nil
	}
	if message.Usage == nil {
		if message.Model != "" && (row.Type == "assistant" || message.Role == "assistant") {
			return e, false, errors.New("assistant record missing token usage")
		}
		return e, false, nil
	}
	if row.Type == "" && message.Role != "assistant" && (message.ID == "" || message.Model == "") {
		return e, false, nil
	}
	at, err := time.Parse(time.RFC3339Nano, row.Timestamp)
	if err != nil {
		return e, false, errors.New("invalid usage timestamp")
	}
	e.at, e.message, e.request = at.UTC(), message.ID, row.RequestID
	inputs, total, err := message.Usage.counts()
	if err != nil {
		return e, false, err
	}
	e.inputs, e.amounts = inputs, []int64{total}
	// Main-model iterations are already included in the parent totals.
	// advisor_message iterations are billed separately (ccusage issue 1423).
	for _, iteration := range message.Usage.Iterations {
		if iteration.Type == "advisor_message" {
			_, n, err := iteration.counts()
			if err != nil {
				return e, false, fmt.Errorf("advisor usage: %w", err)
			}
			e.amounts = append(e.amounts, n)
		}
	}
	if e.request == "" && e.uuid != "" {
		e.tools, err = claudeToolIDs(message.Content, false)
		if err != nil {
			return e, false, err
		}
	}
	return e, true, nil
}

// Other user content breaks the link even when mixed with a tool result.
func claudeToolIDs(content json.RawMessage, results bool) ([]string, error) {
	if len(content) == 0 || content[0] != '[' {
		return nil, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, err
	}
	var ids []string
	for _, raw := range blocks {
		var block struct {
			Type, ID  string
			ToolUseID string `json:"tool_use_id"`
		}
		if len(raw) > 0 && raw[0] == '{' {
			if err := json.Unmarshal(raw, &block); err != nil {
				return nil, err
			}
		}
		if results {
			if block.Type != "tool_result" || block.ToolUseID == "" {
				return nil, nil
			}
			ids = append(ids, block.ToolUseID)
		} else if block.Type == "tool_use" && block.ID != "" {
			ids = append(ids, block.ID)
		}
	}
	return ids, nil
}

// Prefer the percentages Anthropic reports for the signed-in account. The
// local token estimate remains a fallback when budgets are configured.
func fetchClaude(ctx context.Context, c claudeConfig, cache logCache[claudeEvent], now time.Time) (readings, error) {
	if !c.UsageAPI {
		return fetchClaudeLogs(ctx, c, cache, now)
	}
	out, apiErr := fetchClaudeUsageAPI(ctx, now)
	if apiErr == nil {
		var missing []error
		for _, key := range c.Items {
			if !out[key].Valid {
				missing = append(missing, fmt.Errorf("%s: not reported by the Claude usage API", key))
			}
		}
		return out, errors.Join(missing...)
	}
	if c.FiveHourTokens == 0 && c.WeeklyTokens == 0 {
		return nil, apiErr
	}
	out, err := fetchClaudeLogs(ctx, c, cache, now)
	return out, errors.Join(fmt.Errorf("using the local estimate: %w", apiErr), err)
}

func fetchClaudeLogs(ctx context.Context, c claudeConfig, cache logCache[claudeEvent], now time.Time) (readings, error) {
	dir, err := logPath(c.ProjectsDir, "CLAUDE_CONFIG_DIR", ".claude", "projects")
	if err != nil {
		return nil, err
	}
	events, err := cache.read(ctx, dir, parseClaude)
	if err != nil {
		return nil, err
	}
	return claudeUsage(events, c, now)
}

// Multiple signatures can connect one request across files. For example a
// transcript copy shares its request ID with the original, while a sidechain
// replay has a new request ID and shares its session and message IDs.
type claudeGroups []int

func (g claudeGroups) root(i int) int {
	for g[i] != i {
		g[i] = g[g[i]]
		i = g[i]
	}
	return i
}

func (g claudeGroups) join(i, j int) { g[g.root(i)] = g.root(j) }

type claudeTotal struct {
	at     time.Time
	tokens int64
}

type claudeKey [2]string

func groupClaude(events []claudeEvent, now time.Time) (claudeGroups, error) {
	type timedKey struct {
		session, message string
		at               time.Time
	}
	g := make(claudeGroups, len(events))
	requests := map[claudeKey]int{}
	rewrites := map[timedKey]int{}
	nodes := map[claudeKey]int{}
	toolOwners := map[claudeKey]int{}
	replays := map[claudeKey][]int{}
	for i, e := range events {
		g[i] = i
		if e.at.After(now) {
			continue
		}
		node := claudeKey{e.session, e.uuid}
		parent, linked := nodes[claudeKey{e.session, e.parent}]
		if len(e.amounts) == 0 {
			if !linked {
				continue
			}
			for _, id := range e.tools {
				owner, ok := toolOwners[claudeKey{e.session, id}]
				if !ok || g.root(owner) != g.root(parent) {
					linked = false
					break
				}
			}
			if linked {
				nodes[node] = parent
			}
			continue
		}
		if e.uuid != "" {
			if prev, ok := nodes[node]; ok && events[prev].message == e.message {
				g.join(i, prev)
			}
			nodes[node] = i
		}
		if e.message == "" {
			continue
		}
		if e.request != "" {
			k := claudeKey{e.message, e.request}
			if prev, ok := requests[k]; ok {
				g.join(i, prev)
			}
			requests[k] = i
		} else {
			k := timedKey{e.session, e.message, e.at}
			if prev, ok := rewrites[k]; ok {
				g.join(i, prev)
			}
			rewrites[k] = i
			// Gateways may reuse a message ID for unrelated requests. Across
			// timestamps we require both transcript ancestry and unchanged input.
			if linked && events[parent].request == "" && events[parent].message == e.message && events[parent].inputs == e.inputs {
				g.join(i, parent)
			}
			for _, id := range e.tools {
				toolOwners[claudeKey{e.session, id}] = i
			}
		}
		if e.sidechain {
			k := claudeKey{e.session, e.message}
			replays[k] = append(replays[k], i)
		}
	}
	if err := joinClaudeReplays(events, now, g, replays); err != nil {
		return nil, err
	}
	return g, nil
}

func joinClaudeReplays(events []claudeEvent, now time.Time, groups claudeGroups, replays map[claudeKey][]int) error {
	if len(replays) == 0 {
		return nil
	}

	// Only sidechain replays can match a different request ID. Preserve
	// distinct ordinary requests even when a gateway reused a message ID.
	mains := map[claudeKey]map[int]bool{}
	explicit := map[int]bool{}
	for i, e := range events {
		if e.sidechain || len(e.amounts) == 0 || e.at.After(now) {
			continue
		}
		root := groups.root(i)
		if e.request != "" {
			explicit[root] = true
		}
		k := claudeKey{e.session, e.message}
		if len(replays[k]) == 0 {
			continue
		}
		if mains[k] == nil {
			mains[k] = map[int]bool{}
		}
		mains[k][root] = true
	}
	for k, copies := range replays {
		parents := mains[k]
		// An ordinary requestless rewrite can already be grouped with a
		// requestless replay by its timestamp or transcript UUID. It is an
		// alias for that replay, not a second candidate for its explicit parent.
		hasExplicitParent := false
		for parent := range parents {
			if explicit[parent] {
				hasExplicitParent = true
				break
			}
		}
		if hasExplicitParent {
			for _, i := range copies {
				root := groups.root(i)
				if events[i].request == "" && !explicit[root] {
					delete(parents, root)
				}
			}
		}
		if len(parents) <= 1 {
			continue
		}
		for _, i := range copies {
			if !parents[groups.root(i)] {
				return errors.New("ambiguous Claude sidechain replay")
			}
		}
	}
	for k, copies := range replays {
		parents := mains[k]
		if len(parents) > 1 {
			continue
		}
		target := copies[0]
		for parent := range parents {
			target = parent
		}
		for _, i := range copies {
			groups.join(i, target)
		}
	}
	return nil
}

func uniqueClaude(events []claudeEvent, now time.Time) ([]claudeTotal, error) {
	g, err := groupClaude(events, now)
	if err != nil {
		return nil, err
	}

	type counter struct {
		tokens    int64
		sidechain bool
	}
	type response struct {
		at       time.Time
		counters []counter
	}
	responses := map[int]*response{}
	for i, e := range events {
		if len(e.amounts) == 0 || e.at.After(now) {
			continue
		}
		id := g.root(i)
		r := responses[id]
		if r == nil {
			r = &response{at: e.at}
			responses[id] = r
		}
		if e.at.Before(r.at) {
			r.at = e.at
		}
		for j, n := range e.amounts {
			if j == len(r.counters) {
				r.counters = append(r.counters, counter{n, e.sidechain})
				continue
			}
			old := r.counters[j]
			if (old.sidechain && !e.sidechain) || (old.sidechain == e.sidechain && n > old.tokens) {
				r.counters[j] = counter{n, e.sidechain}
			}
		}
	}
	out := make([]claudeTotal, 0, len(responses))
	for _, r := range responses {
		sum := int64(0)
		for _, c := range r.counters {
			if c.tokens > math.MaxInt64-sum {
				return nil, errors.New("response token total overflow")
			}
			sum += c.tokens
		}
		out = append(out, claudeTotal{r.at, sum})
	}
	return out, nil
}

func claudeUsage(events []claudeEvent, c claudeConfig, now time.Time) (readings, error) {
	unique, err := uniqueClaude(events, now)
	if err != nil {
		return nil, err
	}
	if len(unique) == 0 {
		return nil, errors.New("no Claude assistant usage records")
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i].at.Before(unique[j].at) })
	var weekEnd, blockEnd time.Time
	const week = 7 * 24 * time.Hour
	if c.WeeklyResetAnchor != "" {
		anchor, err := time.Parse(time.RFC3339, c.WeeklyResetAnchor)
		if err != nil {
			return nil, err
		}
		// Use Unix seconds: an anchor may precede or follow the current week.
		period := int64(week / time.Second)
		elapsed := ((now.Unix()-anchor.Unix())%period + period) % period
		weekEnd = time.Unix(now.Unix()-elapsed+period, 0)
	}
	var weekly, block int64
	for _, e := range unique {
		// ccusage starts blocks on a UTC clock hour. Reconstruct the sequence
		// before filtering so a rolling lookback cannot shift today's anchor.
		if blockEnd.IsZero() || e.at.After(blockEnd) {
			blockEnd = e.at.UTC().Truncate(time.Hour).Add(5 * time.Hour)
			block = 0
		}
		if e.tokens > math.MaxInt64-block {
			return nil, errors.New("block token total overflow")
		}
		block += e.tokens
		if !weekEnd.IsZero() && !e.at.Before(weekEnd.Add(-week)) {
			if e.tokens > math.MaxInt64-weekly {
				return nil, errors.New("weekly token total overflow")
			}
			weekly += e.tokens
		}
	}
	if !now.Before(blockEnd) {
		block = 0
		blockEnd = time.Time{}
	}
	out := readings{}
	if c.FiveHourTokens > 0 {
		out["5h"] = usedTokens(block, c.FiveHourTokens, blockEnd)
	}
	if c.WeeklyTokens > 0 && !weekEnd.IsZero() {
		out["week"] = usedTokens(weekly, c.WeeklyTokens, weekEnd)
	}
	for _, key := range c.Items {
		if !out[key].Valid {
			return out, fmt.Errorf("%s: set the token budget and, for weekly usage, weekly_reset_anchor", key)
		}
	}
	return out, nil
}

func usedTokens(n, limit int64, reset time.Time) reading {
	return reading{Valid: true, Estimated: true, Percent: 100 * float64(n) / float64(limit), Reset: reset}
}
