package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDashboardServesTheSameSnapshotInEachFormat(t *testing.T) {
	d := newDashboard()
	server := httptest.NewServer(d)
	defer server.Close()
	get := func(method, path string, wantCode int) ([]byte, http.Header) {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != wantCode {
			t.Fatalf("%s %s returned %d (%v)", method, path, resp.StatusCode, err)
		}
		return b, resp.Header
	}
	_, header := get("GET", "/", 503)
	if header.Get("Retry-After") == "" {
		t.Fatal("no retry interval during initial collection")
	}

	at := time.Now().Add(-time.Minute)
	rows := []string{"zero", "missing", "expired", "fourth", "fifth"}
	values := readings{"zero": measured(0, time.Now().Add(time.Hour)), "expired": measured(48, at)}
	ps := []panel{{ID: "codex", Title: "Codex", Keys: rows, Values: values}}
	d.publish(ps, at, displayConfig{}, false)
	html, header := get("GET", "/", 200)
	if !strings.Contains(header.Get("Content-Type"), "text/html") || !bytes.Contains(html, []byte(">0%<")) || bytes.Count(html, []byte("取得不能")) != 4 {
		t.Fatal("HTML did not distinguish zero from missing and expired data")
	}
	api, _ := get("GET", "/api/status", 200)
	var snap quotaSnapshot
	if err := json.Unmarshal(api, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Groups) != 1 || len(snap.Groups[0].Rows) != len(rows) {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	data := snap.Groups[0].Rows
	if data[0].Percent == nil || *data[0].Percent != 0 || data[1].Percent != nil || data[2].Percent != nil {
		t.Fatal("JSON reported incorrect zero, missing or expired values")
	}
	metrics, _ := get("GET", "/metrics", 200)
	if !strings.Contains(string(metrics), "quota_panel_percent{source=\"codex\",key=\"zero\",direction=\"used\"} 0\n") || strings.Count(string(metrics), "\nquota_panel_percent{") != 1 {
		t.Fatal("metrics emitted the wrong available series")
	}
	var text bytes.Buffer
	if err := writeStatus(&text, snap, "text"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "zero  0%") || strings.Count(text.String(), "取得不能") != 4 {
		t.Fatal("text output did not distinguish measured zero and unavailable data")
	}
	first, _ := get("GET", "/panel.png", 200)
	second, _ := get("GET", "/panel.png?page=2", 200)
	if bytes.Equal(first, second) {
		t.Fatal("second image did not advance the page")
	}
	info, err := png.DecodeConfig(bytes.NewReader(second))
	if err != nil || info.Width != catalogWidth || info.Height != catalogHeight {
		t.Fatalf("invalid panel PNG: %+v, %v", info, err)
	}
	get("GET", "/panel.png?page=3", 400)
	body, header := get("HEAD", "/api/status", 200)
	if len(body) != 0 || header.Get("Cache-Control") != "no-store" {
		t.Fatal("unexpected HEAD response")
	}
	get("POST", "/api/status", 405)
}

func TestDashboardAcceptsDefaultPortHost(t *testing.T) {
	d := newDashboard()
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]"} {
		req := httptest.NewRequest("GET", "http://"+host+"/healthz", nil)
		w := httptest.NewRecorder()
		d.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("Host %q: got %d, want initial-collection 503", host, w.Code)
		}
	}
}

func TestLocalWebBindAndModes(t *testing.T) {
	for _, addr := range []string{":9080", "0.0.0.0:9080", "192.0.2.1:9080", "localhost:65536"} {
		if _, err := loopbackAddress(addr); err == nil {
			t.Errorf("accepted non-loopback or invalid bind address %q", addr)
		}
	}
	for _, addr := range []string{"localhost:9080", "127.0.0.1:0", "[::1]:9080"} {
		if _, err := loopbackAddress(addr); err != nil {
			t.Errorf("rejected loopback address %q: %v", addr, err)
		}
	}
	// Validate mutually exclusive live/demo modes before reading config or
	// attempting to talk to any real devices.
	err := run(context.Background(), []string{"-once", "-demo", "-config", filepath.Join(t.TempDir(), "missing")}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-once") {
		t.Fatalf("expected live/demo mode conflict, got %v", err)
	}

	for _, args := range [][]string{
		{"-demo", "-status", "json", "-page", "2"},
		{"-status", "text", "-web", "127.0.0.1:0"},
		{"-headless", "-preview", "file.png"},
	} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Errorf("accepted incompatible options %q", args)
		}
	}
}
