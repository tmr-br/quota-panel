package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestStaticFrameAndPanelRecovery(t *testing.T) {
	c, err := readConfig("")
	if err != nil {
		t.Fatal(err)
	}
	r, err := newRenderer(catalogSize())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1780000000, 0)
	ps := demoPanels(c, now)
	if len(ps) != 4 || len(ps[0].Keys) != 2 {
		t.Fatalf("bad defaults: %+v", ps)
	}
	frame, err := r.frame(ps, now, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := png.DecodeConfig(bytes.NewReader(frame))
	if err != nil || info.Width != 1920 || info.Height != 462 {
		t.Fatal(info, err)
	}
	again, err := r.frame(ps, now, true, 0)
	if err != nil || !bytes.Equal(again, frame) {
		t.Fatal("render is not deterministic", err)
	}
	var connected, missing, fail bool
	uploads := 0
	size := catalogSize()
	snapshot := &scene{panels: ps, at: now, demo: true}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.Method + " " + req.URL.Path {
		case "GET /devices":
			products := []product{{Key: "0416:5302"}}
			if !missing {
				products = append(products, product{Key: "0416:5409", VID: 0x416, PID: 0x5409})
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "products": products})
		case "GET /devices/0416:5409/state":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "connected": connected, "wire": "ly", "product": "Trofeo Vision 9.16 LCD", "pm_byte": 66, "sub_byte": 3, "resolution": []int{size.X, size.Y}})
		case "POST /devices/0416:5409/connect":
			connected = true
			io.WriteString(w, `{"ok":true}`)
		case "POST /devices/0416:5409/display/send-image":
			file, _, err := req.FormFile("image")
			if err != nil {
				t.Error(err)
				http.Error(w, "file", 400)
				return
			}
			defer file.Close()
			data, err := io.ReadAll(file)
			if err != nil || (size == catalogSize() && !bytes.Equal(data, frame)) {
				t.Error("wrong image", err)
			}
			header, e := png.DecodeConfig(bytes.NewReader(data))
			if e != nil || header.Width != size.X || header.Height != size.Y {
				t.Errorf("uploaded PNG dimensions %v, expected %v", header, size)
			}
			uploads++
			json.NewEncoder(w).Encode(map[string]bool{"ok": !fail})
		default:
			t.Errorf("unexpected endpoint %s", req.URL.Path)
			http.NotFound(w, req)
		}
	}))
	defer s.Close()
	var d delivery
	send := func(wantError bool) {
		t.Helper()
		_, err := d.sync(context.Background(), newTrcc(s.URL), "", snapshot)
		if (err != nil) != wantError {
			t.Fatalf("unexpected send result %v", err)
		}
	}
	send(false)
	send(false)
	if uploads != 1 {
		t.Fatal("duplicate upload")
	}
	missing = true
	send(true)
	missing = false
	fail = true
	send(true)
	fail = false
	send(false)
	connected = false
	send(false)
	if uploads != 4 {
		t.Fatalf("uploads=%d, want 4", uploads)
	}
	size = image.Pt(1920, 440)
	send(false)
	send(false)
	size = image.Pt(1280, 480)
	send(false)
	if uploads != 6 {
		t.Fatalf("geometry changes require exactly one fresh upload, got %d", uploads)
	}
}

func TestRejectOtherLYPanelsAndUnverifiedCatalogName(t *testing.T) {
	for _, test := range []struct {
		name, product, key string
		pm, sub            int
	}{
		{"11.3 sharing the same USB ID", "Trofeo Vision 11.3 LCD", "0416:5408", 69, 2},
		{"catalog name hides a different SKU", "Trofeo Vision 9.16 LCD", "0416:5409", 65, 0},
		{"different PM 66 SKU", "Trofeo Vision 9.16 LCD", "0416:5409", 66, 1},
		{"catalog name without fingerprint", "Trofeo Vision 9.16 LCD", "0416:5408", 0, 0},
		{"model mismatch despite fingerprint", "Trofeo Vision 11.3 LCD", "0416:5408", 65, 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/devices":
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "products": []product{{Key: test.key, Native: []int{1920, 462}}}})
				case devicePath(test.key) + "/state":
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "connected": true, "product": test.product, "wire": "ly", "pm_byte": test.pm, "sub_byte": test.sub, "resolution": []int{1920, 440}})
				default:
					t.Errorf("unexpected %s %s for unidentified panel", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer s.Close()
			var d delivery
			if _, err := d.sync(context.Background(), newTrcc(s.URL), test.key, &scene{}); err == nil {
				t.Fatal("unverified panel accepted")
			}
			size, _ := newTrcc(s.URL).previewSize(context.Background(), test.key)
			if size != catalogSize() {
				t.Fatalf("offline preview used wrong panel geometry %v", size)
			}
		})
	}
}

func TestWatchResumesAfterSubscriptionGap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var attempts atomic.Int32
	sockets := make(chan *websocket.Conn, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		sockets <- conn
		<-conn.CloseRead(ctx).Done()
	}))
	defer s.Close()
	w := &panelWatch{wake: make(chan struct{}, 1)}
	go w.follow(ctx, s.URL, "0416:5408@1-2", time.Millisecond)
	wait := func(epoch uint64) {
		t.Helper()
		select {
		case <-w.wake:
			if n := w.epoch.Load(); n != epoch {
				t.Fatalf("connection epoch %d, want %d", n, epoch)
			}
		case <-ctx.Done():
			t.Fatal("watch did not invalidate the acknowledgement")
		}
	}
	wait(1)
	conn := <-sockets
	for _, msg := range []string{`{"event":"DeviceDisconnected","fields":{"key":"0416:5408@1-3"}}`, `{"event":"DeviceConnected","fields":{"key":"0416:5408@1-2"}}`} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatal(err)
		}
	}
	wait(2)
	cancel()
}

func TestSendChecksFreshnessAfterHandshake(t *testing.T) {
	now := time.Now()
	p := panel{ID: "codex", Title: "Codex", Keys: []string{"primary"}, Values: readings{"primary": measured(61, now.Add(-time.Second))}}
	stale := &scene{panels: []panel{p}, at: now.Add(-2 * time.Minute)}
	current := &scene{panels: []panel{p}, at: now}
	want, err := current.pngAt(catalogSize())
	if err != nil {
		t.Fatal(err)
	}
	old, err := stale.pngAt(catalogSize())
	if err != nil || bytes.Equal(want, old) {
		t.Fatal("fixture did not show a quota reset", err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/devices":
			io.WriteString(w, `{"ok":true,"products":[{"key":"0416:5408"}]}`)
		case "/devices/0416:5408/state":
			io.WriteString(w, `{"ok":true,"connected":true,"product":"Trofeo Vision 9.16 LCD","wire":"ly","pm_byte":65,"sub_byte":5,"resolution":[1920,462]}`)
		case "/devices/0416:5408/display/send-image":
			f, _, err := r.FormFile("image")
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			b, err := io.ReadAll(f)
			if err != nil || !bytes.Equal(want, b) {
				t.Error("sent an expired reading after handshake", err)
			}
			io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	var d delivery
	if _, err = d.sync(context.Background(), newTrcc(s.URL), "", stale); err != nil {
		t.Fatal(err)
	}
}
