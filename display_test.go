package main

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDisplayOptionsApplyToExportedPNG(t *testing.T) {
	now := time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)
	p := panel{ID: "opencode", Title: "OpenCode Go", Keys: []string{"a"}, Values: readings{"a": {Percent: -0.3, Valid: true, Estimated: true, Reset: now.Add(time.Hour)}}}
	if displayLabel(p, "a") != "est. · a" || percentText(-0.3) != "<0" {
		t.Fatal("ambiguous model estimate or deficit")
	}
	size := image.Pt(640, 154)
	render := func(display displayConfig) image.Image {
		t.Helper()
		if err := display.validate(); err != nil {
			t.Fatal(err)
		}
		s := scene{panels: []panel{p}, at: now, display: display}
		data, err := s.pngAt(size)
		if err != nil {
			t.Fatal(err)
		}
		im, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		return im
	}
	a := render(displayConfig{Timezone: "UTC"})
	b := render(displayConfig{Timezone: "UTC", Rotate180: true})
	tz := render(displayConfig{Timezone: "Asia/Tokyo"})
	contrast := render(displayConfig{Timezone: "UTC", HighContrast: true})
	sameTime := true
	for y := 0; y < size.Y; y++ {
		for x := 0; x < size.X; x++ {
			if a.At(x, y) != b.At(size.X-1-x, size.Y-1-y) {
				t.Fatalf("rotation at %d,%d", x, y)
			}
			if a.At(x, y) != tz.At(x, y) {
				sameTime = false
			}
		}
	}
	if sameTime || a.At(0, 0) == contrast.At(0, 0) {
		t.Fatal("timezone or contrast had no visible effect")
	}
}

func TestConfigInitAndReloadBoundaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "options.toml")
	if err := initConfig(path); err != nil {
		t.Fatal(err)
	}
	c, err := readConfig(path)
	if err != nil || c.Insights.RetentionDays != 7 {
		t.Fatal(c, err)
	}
	if err := os.WriteFile(path, []byte("display_order=[]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := initConfig(path); err == nil {
		t.Fatal("overwrote existing settings")
	}
	c, err = c.reload(path)
	if err != nil || len(c.Order) != 0 {
		t.Fatal(c, err)
	}
	for _, bad := range []string{"", "[display]\ntimezone='No/Such_Zone'", "[display]\npage_seconds=-1", "[insights]\nalert_remaining_percent=101"} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = c.reload(path); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if err := run(context.Background(), []string{"-config", path, "-check-config"}, io.Discard); err == nil {
		t.Fatal("CLI accepted invalid settings")
	}
}

func TestPreviewStreamsPNGWithoutHardware(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-demo", "-preview", "-", "-preview-size", "480x120"}, &out); err != nil {
		t.Fatal(err)
	}
	header, err := png.DecodeConfig(&out)
	if err != nil || header.Width != 480 || header.Height != 120 {
		t.Fatal(header, err)
	}
}
