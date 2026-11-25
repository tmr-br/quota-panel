package main

import (
	"context"
	"log"
	"slices"
	"sync"
	"time"
)

type sources struct {
	claude logCache[claudeEvent]
	codex  logCache[codexEvent]
}

func newSources() sources { return sources{logCache[claudeEvent]{}, logCache[codexEvent]{}} }

// Each provider owns its log cache. Return a complete new snapshot of the
// given panels after the independent fetches finish, preserving their order.
func (s sources) collect(ctx context.Context, c config, ps []panel, now time.Time) ([]panel, []error) {
	ps = append([]panel(nil), ps...)
	problems := make([]error, len(ps))
	var wg sync.WaitGroup
	for i := range ps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := ps[i]
			var values readings
			var err error
			switch p.ID {
			case "claude":
				values, err = fetchClaude(ctx, c.Claude, s.claude, now)
			case "codex":
				values, err = fetchCodex(ctx, c.Codex, s.codex, now)
			case "opencode":
				values, err = fetchOpenCode(ctx, c.OpenCode, now)
			case "antigravity":
				values, err = fetchAntigravity(ctx, p.Keys, now, fetchRPC)
			case "system":
				values, err = fetchSystem(ctx, p.Keys, systemRunner)
			}
			p.Values = values
			p.FetchFailed = err != nil
			p.FetchedAt = acquisitionTime(p, time.Now())
			ps[i] = p
			problems[i] = err
		}()
	}
	wg.Wait()
	return ps, problems
}

// Use source snapshot time when supplied (Codex log records), otherwise the
// time a valid reading was collected. Errors without data never advance it.
func acquisitionTime(p panel, collected time.Time) time.Time {
	var at time.Time
	for _, key := range p.Keys {
		v := p.Values[key]
		if !v.Valid {
			continue
		}
		t := v.ObservedAt
		if t.IsZero() {
			t = collected
		}
		if at.IsZero() || t.Before(at) {
			at = t
		}
	}
	return at
}

func preserveAcquisitionTimes(ps []panel, last map[string]time.Time) {
	for i := range ps {
		p := &ps[i]
		if p.FetchedAt.IsZero() && p.FetchFailed {
			p.FetchedAt = last[p.ID]
		} else if !p.FetchedAt.IsZero() {
			last[p.ID] = p.FetchedAt
		}
	}
}

type diagnostics map[string]string

func (d diagnostics) record(id string, err error) {
	if err == nil {
		if _, ok := d[id]; ok {
			log.Printf("%s recovered", id)
			delete(d, id)
		}
		return
	}
	msg := err.Error()
	if prev, ok := d[id]; !ok || prev != msg {
		log.Printf("%s: %s", id, msg)
		d[id] = msg
	}
}
func (d diagnostics) snapshot(ctx context.Context, s sources, c config, want []panel) []panel {
	ps, problems := s.collect(ctx, c, want, time.Now())
	for i, p := range ps {
		d.record(p.Title, problems[i])
	}
	return ps
}

func nextExpiration(ps []panel, now time.Time) time.Time {
	var first time.Time
	for _, p := range ps {
		for _, key := range p.Keys {
			v := p.Values[key]
			end := v.expiresAt()
			if !v.Valid || !end.After(now) {
				continue
			}
			if p.ID == "claude" && v.Reset.After(now) {
				// resetText uses ceiling minutes, then displays only whole
				// hours for windows of a day or longer.
				lastMinute := (v.Reset.Sub(now) - 1) / time.Minute
				if lastMinute+1 >= 24*60 {
					lastMinute = (lastMinute+1)/60*60 - 1
				}
				if change := v.Reset.Add(-lastMinute * time.Minute); change.Before(end) {
					end = change
				}
			}
			if first.IsZero() || end.Before(first) {
				first = end
			}
		}
	}
	return first
}

// splitPanels separates the cheap local System column from the API-backed
// quota columns, which refresh on their own schedules.
func splitPanels(c config) (quota []panel, system *panel) {
	for _, p := range panels(c) {
		if p.ID == "system" {
			system = &p
		} else {
			quota = append(quota, p)
		}
	}
	return quota, system
}

// revalidate is stale-while-revalidate for one column: rows the new fetch
// could not read keep their last good reading, together with the time it was
// acquired, so the as-of stamp shows how old the column is. A column that has
// never been read stays unavailable.
func revalidate(prev panel, had bool, fresh panel, collected time.Time) panel {
	values := readings{}
	for k, v := range fresh.Values {
		if v.Valid && v.ObservedAt.IsZero() {
			v.ObservedAt = collected
		}
		values[k] = v
	}
	if had {
		for _, k := range fresh.Keys {
			if old, ok := prev.Values[k]; !values[k].Valid && ok && old.Valid {
				values[k] = old
			}
		}
	}
	fresh.Values = values
	if at := acquisitionTime(fresh, collected); !at.IsZero() {
		fresh.FetchedAt = at
	} else if had {
		fresh.FetchedAt = prev.FetchedAt
	}
	return fresh
}

// nextRowExpiry is the first future reset or cache expiry among readings;
// the quota loop refetches then instead of waiting a full interval.
func nextRowExpiry(ps []panel, now time.Time) time.Time {
	var first time.Time
	for _, p := range ps {
		for _, k := range p.Keys {
			v := p.Values[k]
			if end := v.expiresAt(); v.Valid && end.After(now) && (first.IsZero() || end.Before(first)) {
				first = end
			}
		}
	}
	return first
}

// Collection owns its caches. Quota sources run one complete cycle every
// interval_seconds (sooner when a displayed reading reaches its reset); the
// System column runs on its own short interval. Each finished fetch replaces
// only its own columns in the displayed snapshot. Nothing is published until
// every enabled column has been fetched once, so the panel never shows a
// placeholder frame. publish receives the displayed panels, and the raw
// quota snapshot (nil after a System-only refresh) for history and alerts.
func pollSources(ctx context.Context, path string, c config, publish func(cfg config, shown, raw []panel, at time.Time)) {
	s, d := newSources(), diagnostics{}
	var mu sync.Mutex
	cur := c
	shown := map[string]panel{}
	last := map[string]time.Time{}
	quotaDone, systemDone, published := false, false, false
	kick := make(chan struct{}, 1)

	// emit runs with mu held.
	emit := func(raw []panel) {
		_, system := splitPanels(cur)
		if !published && (!quotaDone || (system != nil && !systemDone)) {
			return
		}
		ps := panels(cur)
		for i, p := range ps {
			if v, ok := shown[p.ID]; ok {
				v.Title, v.Subtitle, v.Keys = p.Title, p.Subtitle, p.Keys
				ps[i] = v
			}
		}
		published = true
		if publish != nil {
			publish(cur, ps, raw, time.Now())
		}
	}

	sysDiag := diagnostics{} // the System loop logs on its own
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			mu.Lock()
			cfg := cur
			mu.Unlock()
			_, system := splitPanels(cfg)
			if system != nil {
				ps := sysDiag.snapshot(ctx, s, cfg, []panel{*system})
				if ctx.Err() != nil {
					return
				}
				mu.Lock()
				prev, had := shown["system"]
				shown["system"] = revalidate(prev, had, ps[0], time.Now())
				systemDone = true
				emit(nil)
				mu.Unlock()
			}
			timer := time.NewTimer(time.Duration(max(cfg.System.Interval, 1)) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-kick:
				timer.Stop()
			case <-timer.C:
			}
		}
	}()

	for ctx.Err() == nil {
		start := time.Now()
		loaded, err := c.reload(path)
		d.record("TOML", err)
		if err == nil {
			c = loaded
			mu.Lock()
			old := cur
			cur = loaded
			mu.Unlock()
			if !old.System.equal(loaded.System) || !slices.Equal(old.Order, loaded.Order) {
				select {
				case kick <- struct{}{}:
				default:
				}
			}
		}
		quota, _ := splitPanels(c)
		ps := d.snapshot(ctx, s, c, quota)
		preserveAcquisitionTimes(ps, last)
		if ctx.Err() != nil {
			return
		}
		collected := time.Now()
		mu.Lock()
		var view []panel
		for _, p := range ps {
			prev, had := shown[p.ID]
			shown[p.ID] = revalidate(prev, had, p, collected)
			view = append(view, shown[p.ID])
		}
		quotaDone = true
		emit(append([]panel{}, ps...))
		mu.Unlock()
		next := start.Add(time.Duration(c.Interval) * time.Second)
		if soon := nextRowExpiry(view, collected); !soon.IsZero() && soon.Add(time.Second).Before(next) {
			next = soon.Add(time.Second)
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

type panelUpdate struct {
	config config
	panels []panel
	quota  bool // includes a fresh quota cycle (advances auto paging)
}

// observerFunc receives every published snapshot: the displayed panels and,
// after a quota cycle, the raw quota readings (nil after a System refresh).
type observerFunc func(cfg config, shown, raw []panel, at time.Time)

func hasMultiplePages(ps []panel) bool {
	for _, p := range ps {
		if len(p.Keys) > 4 {
			return true
		}
	}
	return false
}

func serve(ctx context.Context, path string, c config, observer observerFunc, headless bool) error {
	if headless {
		pollSources(ctx, path, c, observer)
		return nil
	}
	updates := make(chan panelUpdate, 1)
	stopped := make(chan struct{})
	go func(initial config) {
		defer close(stopped)
		pollSources(ctx, path, initial, func(cfg config, ps, raw []panel, at time.Time) {
			// A busy REST endpoint only needs the latest scene. It must not
			// delay acquisition or the observer that records every snapshot.
			update := panelUpdate{cfg, ps, raw != nil}
			select {
			case updates <- update:
			default:
				select {
				case old := <-updates:
					update.quota = update.quota || old.quota
				default:
				}
				updates <- update // sole producer, one vacant slot
			}
			if observer != nil {
				observer(cfg, ps, raw, at)
			}
		})
	}(c)
	defer func() { <-stopped }()

	t := newTrcc(c.TrccURL)
	watch := newWatch(ctx, c.TrccURL, c.DeviceKey)
	defer func() { watch.stop() }()
	d := diagnostics{}
	ack := delivery{watch: watch}
	var page uint64
	var expires, nextPage time.Time
	var haveSnapshot bool
	now := time.Now()
	ps := panels(c)
	frame := &scene{panels: ps, at: c.Display.local(now), display: c.Display}
	// Nothing is sent until the first complete snapshot arrives: the panel
	// keeps its current image rather than showing a frame of placeholders.
	var nextProbe time.Time
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for ctx.Err() == nil {
		now := time.Now()
		paged := !nextPage.IsZero() && !now.Before(nextPage)
		if paged {
			page++
			nextPage = now.Add(time.Duration(c.Display.PageSeconds) * time.Second)
		}
		if paged || (!expires.IsZero() && !now.Before(expires)) {
			frame = &scene{panels: ps, at: c.Display.local(now), display: c.Display, page: page}
			expires = nextExpiration(ps, now)
			nextProbe = time.Time{}
		}
		if haveSnapshot && !now.Before(nextProbe) {
			msg, err := ack.sync(ctx, t, c.DeviceKey, frame)
			d.record("trcc", err)
			if msg != "" {
				log.Print(msg)
			}
			nextProbe = time.Now().Add(10 * time.Second)
		}
		next := nextProbe
		if !haveSnapshot {
			next = now.Add(time.Hour)
		}
		for _, deadline := range []time.Time{expires, nextPage} {
			if !deadline.IsZero() && deadline.Before(next) {
				next = deadline
			}
		}
		timer.Reset(time.Until(next))
		select {
		case <-ctx.Done():
			return nil
		case u := <-updates:
			now = time.Now()
			newConfig := u.config
			if c.TrccURL != newConfig.TrccURL || c.DeviceKey != newConfig.DeviceKey {
				watch.stop()
				watch = newWatch(ctx, newConfig.TrccURL, newConfig.DeviceKey)
				t = newTrcc(newConfig.TrccURL)
				ack = delivery{watch: watch}
			}
			switch {
			case !hasMultiplePages(u.panels):
				nextPage, page = time.Time{}, 0
			case newConfig.Display.PageSeconds == 0:
				nextPage = time.Time{}
				if haveSnapshot && u.quota {
					page++
				}
			case nextPage.IsZero() || c.Display.PageSeconds != newConfig.Display.PageSeconds:
				nextPage = now.Add(time.Duration(newConfig.Display.PageSeconds) * time.Second)
			}
			c, ps = newConfig, u.panels
			haveSnapshot = true
			frame = &scene{panels: ps, at: c.Display.local(now), display: c.Display, page: page}
			expires = nextExpiration(ps, now)
			nextProbe = time.Time{}
		case <-watch.wake:
			nextProbe = time.Time{}
		case <-timer.C:
		}
	}
	return nil
}
