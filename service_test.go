package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
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
			io.WriteString(w, `{"ok":true,"connected":true,"product":"Trofeo Vision 9.16 LCD","wire":"ly","pm_byte":65,"sub_byte":5,"resolution":[1920,462]}`)
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
	go func() { done <- serve(ctx, path, c, nil, false) }()
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

func TestSlowAcquisitionDoesNotHoldUpReplugOrExpiry(t *testing.T) {
	root := t.TempDir()
	t.Setenv("QUOTA_PANEL_TEST_DIR", root)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Hold the first snapshot until its loading frame arrives, then hold
	// the second open past the Codex reset. Cancellation stops the subprocess.
	script := `#!/bin/sh
if [ -f "$QUOTA_PANEL_TEST_DIR/first" ]; then
	touch "$QUOTA_PANEL_TEST_DIR/busy"
	exec sleep 30
fi
touch "$QUOTA_PANEL_TEST_DIR/first"
while [ ! -f "$QUOTA_PANEL_TEST_DIR/continue" ]; do sleep 0.1; done
echo '{}'
`
	if err := os.WriteFile(filepath.Join(root, "sqlite3"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	// Use the real credential reader so the fake sqlite3 is the slow provider.
	writeFixture(t, filepath.Join(root, "opencode.db"), "")
	t.Setenv("OPENCODE_DB", filepath.Join(root, "opencode.db"))
	readOpenCredential = loadOpenCredential
	defer func() {
		readOpenCredential = func(context.Context) ([]byte, error) { return nil, errors.New("test: no OpenCode sign-in") }
	}()
	const key = "0416:5408@1-2"
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	type upload struct {
		data []byte
		at   time.Time
	}
	images := make(chan upload, 12)
	sockets := make(chan *websocket.Conn, 2)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			sockets <- ws
			<-ws.CloseRead(ctx).Done()
		case "/devices":
			io.WriteString(w, `{"ok":true,"products":[{"key":"0416:5408@1-2"},{"key":"0416:5408@1-3"}]}`)
		case devicePath(key) + "/state":
			io.WriteString(w, `{"ok":true,"connected":true,"product":"Trofeo Vision 9.16 LCD","wire":"ly","pm_byte":65,"sub_byte":5,"resolution":[1920,462]}`)
		case devicePath(key) + "/display/send-image":
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
			images <- upload{b, time.Now()}
			io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	path := filepath.Join(root, "settings.toml")
	writeFixture(t, path, fmt.Sprintf("interval_seconds=1\ntrcc_url=%q\ndevice_key=%q\ndisplay_order=['codex','opencode']\n[codex]\nsessions_dir=%q\nitems=['primary']\n", s.URL, key, root))
	c, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	reset := now.Truncate(time.Second).Add(5 * time.Second)
	writeFixture(t, filepath.Join(root, "session.jsonl"), fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":61,"window_minutes":300,"resets_at":%d}}}}`+"\n", now.Add(-time.Second).Format(time.RFC3339), reset.Unix()))
	done := make(chan error, 1)
	go func() { done <- serve(ctx, path, c, nil, false) }()
	defer func() {
		cancel()
		if e := <-done; e != nil {
			t.Error(e)
		}
	}()
	nextImage := func() upload {
		t.Helper()
		select {
		case u := <-images:
			return u
		case <-ctx.Done():
			t.Fatal("timed out waiting for panel upload")
			return upload{}
		}
	}
	pending := nextImage()
	if err := os.WriteFile(filepath.Join(root, "continue"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	first := nextImage()
	if bytes.Equal(pending.data, first.data) {
		t.Fatal("the initial frame did not transition from unavailable to the Codex reading")
	}
	var ws *websocket.Conn
	select {
	case ws = <-sockets:
	case <-ctx.Done():
		t.Fatal("missing event subscription")
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "busy")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("provider never started its second collection")
		case <-time.After(20 * time.Millisecond):
		}
	}
	// The upstream physical hotplug monitor publishes the bare model key even
	// when the configured REST address has a port suffix.
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"event":"DeviceDetached","fields":{"key":"0416:5408"}}`)); err != nil {
		t.Fatal(err)
	}
	replug := nextImage()
	if !bytes.Equal(replug.data, first.data) || !replug.at.Before(reset) {
		t.Fatal("fast replug did not resend the current frame while collecting")
	}
	expired := nextImage()
	if bytes.Equal(expired.data, first.data) || expired.at.Before(reset) {
		t.Fatal("expired Codex usage stayed on the panel while collecting")
	}
}

func TestHeadlessPublishesWithoutContactingTrcc(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("headless mode contacted %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer s.Close()
	path := filepath.Join(t.TempDir(), "settings.toml")
	writeFixture(t, path, fmt.Sprintf("trcc_url=%q\ndisplay_order=[]\n", s.URL))
	c, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	calls := 0
	err = serve(ctx, path, c, func(cfg config, ps []panel, at time.Time) {
		calls++
		if cfg.TrccURL != s.URL || len(ps) != 0 || at.IsZero() {
			t.Error("unexpected published snapshot")
		}
		cancel()
	}, true)
	if err != nil || calls != 1 {
		t.Fatalf("observer calls=%d error=%v", calls, err)
	}
}

func TestUnresponsivePanelDoesNotBlockSnapshotObserver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/devices" {
			<-r.Context().Done()
			return
		}
		http.NotFound(w, r)
	}))
	defer s.Close()
	path := filepath.Join(t.TempDir(), "settings.toml")
	writeFixture(t, path, fmt.Sprintf("trcc_url=%q\ninterval_seconds=1\ndisplay_order=[]\n", s.URL))
	c, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	err = serve(ctx, path, c, func(config, []panel, time.Time) {
		calls++
		if calls == 3 {
			cancel()
		}
	}, false)
	if err != nil || calls != 3 {
		t.Fatalf("received %d snapshots while REST blocked: %v", calls, err)
	}
}

func TestClaudeCountdownWakesOnlyWhenDisplayedTextChanges(t *testing.T) {
	now := time.Unix(1780000000, 0)
	for _, tc := range []struct{ remain, wait time.Duration }{
		{90 * time.Second, 30 * time.Second},
		{time.Minute + time.Nanosecond, time.Nanosecond},
		{24 * time.Hour, time.Minute},
		{48*time.Hour + 15*time.Minute, 16 * time.Minute},
	} {
		r := measured(55, now.Add(tc.remain))
		p := panel{ID: "claude", Keys: []string{"5h"}, Values: readings{"5h": r}}
		change := nextExpiration([]panel{p}, now)
		if !change.Equal(now.Add(tc.wait)) {
			t.Fatalf("remaining %v: next update in %v, want %v", tc.remain, change.Sub(now), tc.wait)
		}
		if resetText(p.ID, r, change.Add(-time.Nanosecond)) == resetText(p.ID, r, change) {
			t.Fatalf("countdown didn't change at deadline %v", change)
		}
		p.ID = "codex"
		if n := nextExpiration([]panel{p}, now); !n.Equal(r.Reset) {
			t.Fatalf("absolute reset erroneously scheduled a countdown: %v", n)
		}
	}
}
