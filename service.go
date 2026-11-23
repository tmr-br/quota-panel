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
			if v.Valid && v.Reset.After(now) && (first.IsZero() || v.Reset.Before(first)) {
				first = v.Reset
			}
		}
	}
	return first
}

func serve(ctx context.Context, path string, c config) error {
	s := newSources()
	d := diagnostics{}
	var ack delivery
	t := newTrcc(c.TrccURL)
	watch := newWatch(ctx, c.TrccURL, c.DeviceKey)
	defer func() { watch.stop() }()
	var epoch, page uint64
	var nextFetch, nextProbe, expires time.Time
	var ps []panel
	var frame *scene
	for ctx.Err() == nil {
		now := time.Now()
		fetched := !now.Before(nextFetch)
		if fetched {
			loaded, err := readConfig(path)
			d.record("TOML", err)
			if err == nil {
				if c.TrccURL != loaded.TrccURL || c.DeviceKey != loaded.DeviceKey {
					watch.stop()
					watch = newWatch(ctx, loaded.TrccURL, loaded.DeviceKey)
					t = newTrcc(loaded.TrccURL)
					ack = delivery{}
					epoch = 0
				}
				c = loaded
			}
			ps = d.snapshot(ctx, s, c)
			now = time.Now()
			nextFetch = now.Add(time.Duration(c.Interval) * time.Second)
			if frame != nil {
				page++
			}
		}
		if ctx.Err() != nil {
			break
		}
		if fetched || (!expires.IsZero() && !expires.After(now)) {
			frame = &scene{panels: ps, at: now, page: page}
			expires = nextExpiration(ps, now)
			nextProbe = time.Time{}
		}
		if !now.Before(nextProbe) {
			if g := watch.epoch.Load(); g != epoch {
				ack.acknowledged = false
				epoch = g
			}
			msg, err := ack.sync(ctx, t, c.DeviceKey, frame)
			d.record("trcc", err)
			if msg != "" {
				log.Print(msg)
			}
			nextProbe = time.Now().Add(10 * time.Second)
		}
		next := nextFetch
		if nextProbe.Before(next) {
			next = nextProbe
		}
		if !expires.IsZero() && expires.Before(next) {
			next = expires
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-watch.wake:
			timer.Stop()
			nextProbe = time.Time{}
		case <-timer.C:
		}
	}
	return nil
}
