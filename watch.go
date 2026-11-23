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
	go w.follow(ctx, base, key)
	return w
}
func (w *panelWatch) notify(lost bool) {
	if lost {
		w.epoch.Add(1)
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}
func (w *panelWatch) follow(ctx context.Context, base, key string) {
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
	for ctx.Err() == nil {
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, _, err := websocket.Dial(dialCtx, u.String(), nil)
		cancel()
		if err == nil {
			w.observe(ctx, conn, key)
			conn.CloseNow()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}
func (w *panelWatch) observe(ctx context.Context, conn *websocket.Conn, key string) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			// Older servers may explicitly reject an unknown event filter.
			if ctx.Err() == nil && websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				w.notify(true)
			}
			return
		}
		var e struct {
			Event  string
			Fields struct{ Key string }
		}
		if json.Unmarshal(data, &e) != nil {
			continue
		}
		id, _, _ := strings.Cut(strings.ToLower(e.Fields.Key), "@")
		if (id != "0416:5408" && id != "0416:5409") || (key != "" && !strings.EqualFold(key, e.Fields.Key)) {
			continue
		}
		switch e.Event {
		case "DeviceDetached", "DeviceDisconnected":
			w.notify(true)
		case "DeviceAttached", "DeviceConnected":
			w.notify(false)
		}
	}
}
