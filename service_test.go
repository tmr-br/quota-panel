package main

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDaemonRecoversAndReloadsWithoutRestart(t *testing.T) {
	var online atomic.Bool
	probes := make(chan struct{}, 20)
	uploads := make(chan []byte, 10)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/devices":
			available := online.Load()
			probes <- struct{}{}
			if !available {
				http.Error(w, "offline", 503)
				return
			}
			io.WriteString(w, `{"ok":true,"products":[{"key":"0416:5408"}]}`)
		case "/devices/0416:5408/state":
			io.WriteString(w, `{"ok":true,"connected":true,"resolution":[1920,462]}`)
		case "/devices/0416:5408/display/send-image":
			f, _, err := r.FormFile("image")
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			b, err := io.ReadAll(f)
			if err != nil {
				t.Error(err)
				return
			}
			uploads <- b
			io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	root := t.TempDir()
	path := filepath.Join(root, "panel.toml")
	cfg := fmt.Sprintf("interval_seconds=1\ntrcc_url=%q\ndisplay_order=['codex']\n[codex]\nsessions_dir=%q\n", s.URL, root)
	writeFixture(t, path, cfg)
	c, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, path, c) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop")
		}
	}()
	probe := func() {
		t.Helper()
		select {
		case <-probes:
		case <-time.After(5 * time.Second):
			t.Fatal("no panel probe")
		}
	}
	upload := func() []byte {
		t.Helper()
		select {
		case b := <-uploads:
			return b
		case <-time.After(5 * time.Second):
			t.Fatal("no upload")
			return nil
		}
	}
	probe()
	online.Store(true)
	first := upload()
	// Discard the discovery that preceded the first successful upload.
	probe()
	writeFixture(t, path, "invalid=[\n")
	probe()
	probe()
	select {
	case <-uploads:
		t.Fatal("malformed TOML discarded the previous good configuration")
	default:
	}
	writeFixture(t, path, fmt.Sprintf("interval_seconds=1\ntrcc_url=%q\ndisplay_order=[]\n", s.URL))
	second := upload()
	info, err := png.DecodeConfig(bytes.NewReader(second))
	if err != nil || info.Width != 1920 || info.Height != 462 || bytes.Equal(first, second) {
		t.Fatal("configuration change not rendered", info, err)
	}
}
