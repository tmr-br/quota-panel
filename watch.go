package main

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// HTTP polling also works on installations without /events. The event stream
// catches a fast USB replug or trcc restart entirely between two probes.
type panelWatch struct {
	wake  chan struct{}
	epoch atomic.Uint64
	stop  context.CancelFunc
}

func newWatch(ctx context.Context, base, key string) *panelWatch {
	ctx, cancel := context.WithCancel(ctx)
	w := &panelWatch{wake: make(chan struct{}, 1), stop: cancel}
	go w.follow(ctx, base, key, 10*time.Second)
	return w
}
func (w *panelWatch) invalidate() {
	w.epoch.Add(1)
	select {
	case w.wake <- struct{}{}:
	default:
	}
}
func (w *panelWatch) follow(ctx context.Context, base, key string, retry time.Duration) {
	u, err := url.Parse(base + "/events")
	if err != nil {
		return
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	q := u.Query()
	q.Set("types", "DeviceAttached,DeviceDetached,DeviceConnected,DeviceDisconnected")
	u.RawQuery = q.Encode()
	var gap bool
	for ctx.Err() == nil {
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, _, err := websocket.Dial(dialCtx, u.String(), nil)
		cancel()
		if err == nil {
			if gap {
				// Hotplug events during a subscription gap are not replayed.
				w.invalidate()
			}
			gap = w.observe(ctx, conn, key)
			conn.CloseNow()
		} else {
			gap = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}
func (w *panelWatch) observe(ctx context.Context, conn *websocket.Conn, key string) bool {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			// Older servers may explicitly reject an unknown event filter.
			if ctx.Err() == nil && websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				w.invalidate()
				return true
			}
			return false
		}
		var e struct {
			Event  string
			Fields struct{ Key string }
		}
		if json.Unmarshal(data, &e) != nil {
			continue
		}
		id, _, scoped := strings.Cut(strings.ToLower(e.Fields.Key), "@")
		if id != "0416:5408" && id != "0416:5409" {
			continue
		}
		switch e.Event {
		case "DeviceAttached", "DeviceDetached":
			// trcc publishes physical events by model, including when units
			// are individually addressed as VID:PID@port in the REST API.
			wantedModel, _, _ := strings.Cut(strings.ToLower(key), "@")
			if key == "" || strings.EqualFold(key, e.Fields.Key) || (!scoped && id == wantedModel) {
				w.invalidate()
			}
		case "DeviceConnected", "DeviceDisconnected":
			if key == "" || strings.EqualFold(key, e.Fields.Key) {
				w.invalidate()
			}
		}
	}
}
