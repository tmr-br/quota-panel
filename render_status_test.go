package main

import (
	"bytes"
	"image"
	"image/draw"
	"image/png"
	"testing"
	"time"

	"golang.org/x/image/font"
)

// Compare the value area of the actual PNG against the expected text in both
// layouts. A different bar, reset time or heading must not mask a bad label.
func TestFrameDistinguishesUnavailableFromMeasuredZero(t *testing.T) {
	r, err := newRenderer(catalogSize())
	if err != nil {
		t.Fatal(err)
	}
	var columns []panel
	for _, keys := range [][]string{{"one"}, {"one", "two", "three", "four"}} {
		for _, values := range []readings{nil, {"one": measured(0, time.Time{})}} {
			columns = append(columns, panel{ID: "codex", Keys: keys, Values: values})
		}
	}
	data, err := r.frame(columns, time.Unix(1700000000, 0), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	const missing = "N/A"
	x0, x1 := 28, 480+28
	end2, end3 := 3*480-28, 4*480-28
	for _, tc := range []struct {
		name  string
		rect  image.Rectangle
		write func(draw.Image)
	}{
		{"large missing", image.Rect(x0, 170, x0+180, 232), func(dst draw.Image) {
			label(dst, x0, 225, missing, r.faces[36], muted)
		}},
		{"large zero", image.Rect(x1, 166, x1+180, 233), func(dst draw.Image) {
			label(dst, x1, 226, "0", r.bold[70], ink)
			label(dst, x1+font.MeasureString(r.bold[70], "0").Ceil()+6, 224, "%", r.bold[32], accents["codex"])
		}},
		{"small missing", image.Rect(end2-140, 125, end2+1, 165), func(dst draw.Image) {
			label(dst, end2-font.MeasureString(r.faces[22], missing).Ceil(), 159, missing, r.faces[22], muted)
		}},
		{"small zero", image.Rect(end3-140, 125, end3+1, 165), func(dst draw.Image) {
			label(dst, end3-font.MeasureString(r.bold[39], "0%").Ceil(), 159, "0%", r.bold[39], ink)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := image.NewRGBA(tc.rect)
			draw.Draw(want, tc.rect, image.NewUniform(background), image.Point{}, draw.Src)
			tc.write(want)
			got := image.NewRGBA(tc.rect)
			draw.Draw(got, tc.rect, frame, tc.rect.Min, draw.Src)
			if !bytes.Equal(got.Pix, want.Pix) {
				t.Fatal("rendered value differs from the expected label")
			}
		})
	}
	for _, ch := range missing {
		if _, ok := r.faces[36].GlyphAdvance(ch); !ok {
			t.Errorf("embedded font lacks %q", ch)
		}
	}
}
