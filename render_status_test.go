package main

import (
	"bytes"
	"image"
	"image/draw"
	"image/png"
	"testing"
	"time"
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

	// The former accent stripe and header divider must not be drawn.
	for _, point := range []image.Point{{28, 38}, {40, 38}, {400, 116}} {
		if frame.At(point.X, point.Y) != background {
			t.Fatalf("unexpected legend or divider at %v", point)
		}
	}
	const missing = "N/A"
	x0, x1 := 32, 480+32
	end2, end3 := 3*480-32, 4*480-32
	for _, tc := range []struct {
		name  string
		rect  image.Rectangle
		write func(draw.Image)
	}{
		{"large missing", image.Rect(x0, 160, x0+180, 202), func(dst draw.Image) {
			label(dst, x0, 192, missing, r.faces[30], muted)
		}},
		{"large zero", image.Rect(x1, 130, x1+260, 208), func(dst draw.Image) {
			label(dst, x1, 198, "100", r.bold[76], ink)
			label(dst, x1+measure(r.bold[76], "100")+1, 198, "%", r.bold[34], accents["codex"])
		}},
		{"small missing", image.Rect(end2-140, 100, end2+1, 134), func(dst draw.Image) {
			label(dst, end2-measure(r.medium[20], missing), 126, missing, r.medium[20], muted)
		}},
		{"small zero", image.Rect(end3-140, 100, end3+1, 135), func(dst draw.Image) {
			label(dst, end3-measure(r.bold[34], "100")-measure(r.medium[20], "%"), 128, "100", r.bold[34], ink)
			label(dst, end3-measure(r.medium[20], "%"), 128, "%", r.medium[20], accents["codex"])
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
		if _, ok := r.faces[30].GlyphAdvance(ch); !ok {
			t.Errorf("embedded font lacks %q", ch)
		}
	}
}

// Latin text uses IBM Plex Sans; Japanese labels still render from the
// embedded fallback, and figures stay tabular so values do not jitter.
func TestFontFallbackAndTabularFigures(t *testing.T) {
	r, err := newRenderer(catalogSize())
	if err != nil {
		t.Fatal(err)
	}
	face := r.faces[17].(fallbackFace)
	if _, ok := face.primary.GlyphAdvance('残'); ok {
		t.Fatal("primary font unexpectedly covers Japanese")
	}
	if _, ok := face.GlyphAdvance('残'); !ok {
		t.Fatal("Japanese fallback missing")
	}
	if measure(face, "残量") == 0 {
		t.Fatal("Japanese label measured as empty")
	}
	if measure(r.bold[76], "11") != measure(r.bold[76], "00") {
		t.Fatal("value figures are not tabular")
	}
}
