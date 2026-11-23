package main

import (
	"context"
	"log"
	"sync"
	"time"
)

type sources struct {
	claude logCache[claudeEvent]
	codex  logCache[codexEvent]
}

func newSources() sources { return sources{logCache[claudeEvent]{}, logCache[codexEvent]{}} }

// Each provider owns its log cache. Publish a complete new snapshot after the
// independent fetches finish, preserving the configured order.
func (s sources) collect(ctx context.Context, c config, now time.Time) ([]panel, []error) {
	ps := panels(c)
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
			}
			p.Values = values
			ps[i] = p
			problems[i] = err
		}()
	}
	wg.Wait()
	return ps, problems
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
func (d diagnostics) snapshot(ctx context.Context, s sources, c config) []panel {
	ps, problems := s.collect(ctx, c, time.Now())
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

// Collection owns its caches and runs one complete cycle at a time. The
// panel loop remains free to handle resets and reconnections during slow I/O.
func pollSources(ctx context.Context, path string, c config, publish func(config, []panel, time.Time)) {
	s, d := newSources(), diagnostics{}
	for ctx.Err() == nil {
		start := time.Now()
		loaded, err := c.reload(path)
		d.record("TOML", err)
		if err == nil {
			c = loaded
		}
		ps := d.snapshot(ctx, s, c)
		if ctx.Err() != nil {
			return
		}
		if publish != nil {
			publish(c, ps, time.Now())
		}
		next := start.Add(time.Duration(c.Interval) * time.Second)
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
}

func hasMultiplePages(ps []panel) bool {
	for _, p := range ps {
		if len(p.Keys) > 4 {
			return true
		}
	}
	return false
}

func serve(ctx context.Context, path string, c config, observer func(config, []panel, time.Time), headless bool) error {
	if headless {
		pollSources(ctx, path, c, observer)
		return nil
	}
	updates := make(chan panelUpdate, 1)
	stopped := make(chan struct{})
	go func(initial config) {
		defer close(stopped)
		pollSources(ctx, path, initial, func(cfg config, ps []panel, at time.Time) {
			// A busy REST endpoint only needs the latest scene. It must not
			// delay acquisition or the observer that records every snapshot.
			update := panelUpdate{cfg, ps}
			select {
			case updates <- update:
			default:
				select {
				case <-updates:
				default:
				}
				updates <- update // sole producer, one vacant slot
			}
			if observer != nil {
				observer(cfg, ps, at)
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
	// A slow first collection must not leave a previous run's numbers on
	// the panel. Quick collections replace this empty frame before upload.
	nextProbe := now.Add(time.Second)
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
		if !now.Before(nextProbe) {
			msg, err := ack.sync(ctx, t, c.DeviceKey, frame)
			d.record("trcc", err)
			if msg != "" {
				log.Print(msg)
			}
			nextProbe = time.Now().Add(10 * time.Second)
		}
		next := nextProbe
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
				if haveSnapshot {
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
