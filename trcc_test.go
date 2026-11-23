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
	"testing"
	"time"
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
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "connected": connected, "resolution": []int{size.X, size.Y}})
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
