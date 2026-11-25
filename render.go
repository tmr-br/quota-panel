package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"sort"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const catalogWidth, catalogHeight = 1920, 462

func catalogSize() image.Point { return image.Pt(catalogWidth, catalogHeight) }

// A snapshot is rendered directly at the active panel's native resolution.
// Cache its PNG across health probes; a different handshake needs new pixels.
type scene struct {
	display displayConfig
	panels  []panel
	at      time.Time
	demo    bool
	page    uint64
	size    image.Point
	png     []byte
}

func (s *scene) pngAt(size image.Point) ([]byte, error) {
	if size == s.size && s.png != nil {
		return s.png, nil
	}
	r, err := newRenderer(size)
	if err != nil {
		return nil, err
	}
	r.display = s.display
	b, err := r.frame(s.panels, s.display.local(s.at), s.demo, s.page)
	if err != nil {
		return nil, err
	}
	s.size, s.png = size, b
	return b, nil
}

// Latin text uses IBM Plex Sans (tabular figures by default, so values do
// not shift as digits change). Glyphs it lacks fall back to the embedded
// Japanese subset.
var (
	//go:embed assets/panel.otf
	japaneseFont []byte
	//go:embed assets/IBMPlexSans-Regular.ttf
	plexRegular []byte
	//go:embed assets/IBMPlexSans-Medium.ttf
	plexMedium []byte
	//go:embed assets/IBMPlexSans-SemiBold.ttf
	plexSemiBold []byte
)

type renderer struct {
	display displayConfig
	size    image.Point
	scale   float64
	faces   map[int]font.Face // regular: reset lines, as of, N/A
	medium  map[int]font.Face // row labels
	bold    map[int]font.Face // semibold: source names and values
}

func newRenderer(size image.Point) (renderer, error) {
	r := renderer{size: size, scale: min(float64(size.X)/catalogWidth, float64(size.Y)/catalogHeight), faces: map[int]font.Face{}, medium: map[int]font.Face{}, bold: map[int]font.Face{}}
	if size.X <= 0 || size.Y <= 0 || size.X > 8192 || size.Y > 8192 || int64(size.X)*int64(size.Y) > 16<<20 {
		return r, fmt.Errorf("invalid trcc canvas %dx%d", size.X, size.Y)
	}
	fallback, err := opentype.Parse(japaneseFont)
	if err != nil {
		return r, err
	}
	for _, set := range []struct {
		data  []byte
		sizes []int
		faces map[int]font.Face
	}{
		{plexRegular, []int{15, 17, 18, 30}, r.faces},
		{plexMedium, []int{20, 22}, r.medium},
		{plexSemiBold, []int{15, 28, 34, 38, 76}, r.bold},
	} {
		f, err := opentype.Parse(set.data)
		if err != nil {
			return r, err
		}
		for _, size := range set.sizes {
			opts := &opentype.FaceOptions{Size: float64(size) * r.scale, DPI: 72, Hinting: font.HintingFull}
			face, err := opentype.NewFace(f, opts)
			if err != nil {
				return r, err
			}
			jp, err := opentype.NewFace(fallback, opts)
			if err != nil {
				return r, err
			}
			set.faces[size] = fallbackFace{face, jp}
		}
	}
	return r, nil
}

// fallbackFace draws from primary and uses fallback only for missing glyphs.
type fallbackFace struct{ primary, fallback font.Face }

func (f fallbackFace) pick(r rune) font.Face {
	if _, ok := f.primary.GlyphAdvance(r); ok {
		return f.primary
	}
	if _, ok := f.fallback.GlyphAdvance(r); ok {
		return f.fallback
	}
	return f.primary
}
func (f fallbackFace) Close() error { f.fallback.Close(); return f.primary.Close() }
func (f fallbackFace) Glyph(dot fixed.Point26_6, r rune) (image.Rectangle, image.Image, image.Point, fixed.Int26_6, bool) {
	return f.pick(r).Glyph(dot, r)
}
func (f fallbackFace) GlyphBounds(r rune) (fixed.Rectangle26_6, fixed.Int26_6, bool) {
	return f.pick(r).GlyphBounds(r)
}
func (f fallbackFace) GlyphAdvance(r rune) (fixed.Int26_6, bool) { return f.pick(r).GlyphAdvance(r) }
func (f fallbackFace) Kern(r0, r1 rune) fixed.Int26_6 {
	if a, b := f.pick(r0), f.pick(r1); a == b {
		return a.Kern(r0, r1)
	}
	return 0
}
func (f fallbackFace) Metrics() font.Metrics { return f.primary.Metrics() }

func (r renderer) paint(c color.RGBA) color.RGBA {
	if !r.display.HighContrast {
		return c
	}
	switch c {
	case background:
		return rgb(0)
	case faint, muted:
		return rgb(0xe0e6ea)
	case ink, soft:
		return rgb(0xffffff)
	case divider:
		return rgb(0x8fa1ac)
	case track:
		return rgb(0x4c5b66)
	default:
		return c
	}
}
func (r renderer) px(n int) int { return max(1, int(math.Round(float64(n)*r.scale))) }

func rgb(v uint32) color.RGBA { return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255} }

// Four steps of emphasis: values and source names (ink), row labels (soft),
// reset lines (muted), and the as-of stamp and page marker (faint).
var (
	background = rgb(0x0c1116)
	divider    = rgb(0x1f2a33)
	track      = rgb(0x1d2730)
	ink        = rgb(0xf3f6f8)
	soft       = rgb(0xc3ced6)
	muted      = rgb(0x8e9daa)
	faint      = rgb(0x6c7b88)
	accents    = map[string]color.RGBA{"claude": rgb(0xf0a07f), "codex": rgb(0x6fd4b8), "antigravity": rgb(0xae9ff7), "opencode": rgb(0x7fbff7), "system": rgb(0xdcc77f)}
	warning    = rgb(0xff6e6e) // system rows past their threshold (see systemWarn)
)

func fill(img draw.Image, x, y, w, h int, c color.Color) {
	draw.Draw(img, image.Rect(x, y, x+w, y+h), image.NewUniform(c), image.Point{}, draw.Src)
}

// pill draws an anti-aliased rounded bar with fully rounded ends.
func pill(img draw.Image, x, y, w, h int, c color.Color) {
	if w <= 0 || h <= 0 {
		return
	}
	fw, fh := float64(w), float64(h)
	rad := min(fw, fh) / 2
	mask := image.NewAlpha(image.Rect(0, 0, w, h))
	const ss = 4
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			n := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px, py := float64(i)+(float64(sx)+0.5)/ss, float64(j)+(float64(sy)+0.5)/ss
					dx, dy := px-max(rad, min(px, fw-rad)), py-max(rad, min(py, fh-rad))
					if dx*dx+dy*dy <= rad*rad {
						n++
					}
				}
			}
			mask.Pix[j*mask.Stride+i] = uint8(n * 255 / (ss * ss))
		}
	}
	draw.DrawMask(img, image.Rect(x, y, x+w, y+h), image.NewUniform(c), image.Point{}, mask, image.Point{}, draw.Over)
}

func label(img draw.Image, x, baseline int, s string, face font.Face, c color.Color) {
	d := font.Drawer{Dst: img, Src: image.NewUniform(c), Face: face, Dot: fixed.P(x, baseline)}
	d.DrawString(s)
}
func measure(f font.Face, s string) int { return font.MeasureString(f, s).Ceil() }
func fit(s string, f font.Face, max int) string {
	if font.MeasureString(f, s).Ceil() <= max {
		return s
	}
	if font.MeasureString(f, "…").Ceil() > max {
		return ""
	}
	runes := []rune(s)
	end := sort.Search(len(runes), func(i int) bool { return font.MeasureString(f, string(runes[:i+1])+"…").Ceil() > max })
	return string(runes[:end]) + "…"
}

// Layout in catalog pixels (1920×462); px scales to the handshake size.
const (
	padX      = 32 // column inner padding
	headBase  = 60 // source name / as-of baseline
	metaBase  = 86 // DEMO and page marker baseline
	bodyTop   = 98
	bodyBot   = 446
	ruleInset = 30
)

func (r renderer) frame(ps []panel, now time.Time, demo bool, page uint64) ([]byte, error) {
	width, height := r.size.X, r.size.Y
	px := r.px
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	fill(img, 0, 0, width, height, r.paint(background))
	if len(ps) == 0 {
		label(img, px(48), height/2+px(10), "Nothing to display", r.faces[30], r.paint(muted))
	}
	for i, p := range ps {
		left, right := i*width/len(ps), (i+1)*width/len(ps)
		if i > 0 {
			fill(img, left, px(ruleInset), px(1), height-2*px(ruleInset), r.paint(divider))
		}
		pad := padX
		if len(ps) >= 5 {
			pad = padX - 6 // narrower columns keep more room for labels
		}
		x, end := left+px(pad), right-px(pad)
		// The source heading is the first row; no direction legend or divider.
		pages := (len(p.Keys) + 3) / 4
		index := int(page % uint64(pages))
		keys := p.Keys[index*4 : min((index+1)*4, len(p.Keys))]
		titleFace, stampFace, metaFace := r.bold[34], r.faces[17], r.bold[15]
		title := fit(p.Title, titleFace, end-x)
		label(img, x, px(headBase), title, titleFace, r.paint(ink))
		stamp := sourceStamp(p.FetchedAt, p.FetchFailed, now)
		var marks []string
		if p.Subtitle != "" {
			marks = append(marks, p.Subtitle)
		}
		if demo {
			marks = append(marks, "DEMO")
		}
		if pages > 1 {
			marks = append(marks, fmt.Sprintf("%d/%d", index+1, pages))
		}
		meta := strings.Join(marks, " · ")
		metaWidth := measure(metaFace, meta)
		if meta != "" {
			label(img, end-metaWidth, px(metaBase), meta, metaFace, r.paint(faint))
		}
		stampColor := r.paint(faint)
		if p.FetchFailed {
			stampColor = r.paint(muted)
		}
		titleWidth, stampWidth := measure(titleFace, title), measure(stampFace, stamp)
		if titleWidth+px(16)+stampWidth <= end-x {
			label(img, end-stampWidth, px(headBase), stamp, stampFace, stampColor)
		} else {
			stamp = fit(stamp, stampFace, end-x-metaWidth-px(12))
			label(img, x, px(metaBase), stamp, stampFace, stampColor)
		}
		span := px(bodyBot) - px(bodyTop)
		if len(p.Keys) <= 2 {
			for j, k := range keys {
				r.largeRow(img, p, k, x, end, px(bodyTop)+span*j/2, now)
			}
		} else {
			for j, k := range keys {
				r.smallRow(img, p, k, x, end, px(bodyTop)+span*j/4, now)
			}
		}
	}
	if r.display.Rotate180 {
		rotateRGBA(img)
	}
	var b bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&b, img); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func percentText(v float64) string {
	// Avoid rounding a tiny remaining allowance to 0 or a partial one to 100.
	if v < 0 && v > -1 {
		return "<0"
	}
	if v > 0 && v < 1 {
		return "<1"
	}
	if v > 99 && v < 100 {
		return ">99"
	}
	return fmt.Sprintf("%.0f", v)
}

// accent is the source colour, or the warning colour for a system row past
// its threshold.
func accent(p panel, key string, v reading) color.RGBA {
	if p.ID == "system" && systemWarn(key, v) {
		return warning
	}
	return accents[p.ID]
}

func (r renderer) bar(img draw.Image, x, y, w, h int, ratio float64, c color.Color) {
	pill(img, x, y, w, h, r.paint(track))
	n := int(math.Round(float64(w) * max(0, min(ratio, 100)) / 100))
	if ratio > 0 && n == 0 {
		n = 1
	}
	pill(img, x, y, n, h, c)
}

// shownAt is the reading a row displays. A last-good reading that has passed
// its reset or cache expiry stays on screen, dimmed, with a "refreshing"
// note until the next fetch replaces it (the quota loop refetches at that
// moment). Only a row that was never read is unavailable.
func (r renderer) shownAt(p panel, k string, now time.Time) (v reading, valueInk, tint color.RGBA, note string) {
	v = p.valueAt(k, now)
	if v.Valid {
		return v, r.paint(ink), accent(p, k, v), resetText(p.ID, v, now)
	}
	if old := p.Values[k]; old.Valid {
		return old, r.paint(muted), r.paint(faint), "reset passed · refreshing"
	}
	return v, color.RGBA{}, color.RGBA{}, ""
}

// Large rows (one or two windows): label, hero value, bar, reset line.
func (r renderer) largeRow(img draw.Image, p panel, k string, x, end, y int, now time.Time) {
	px := r.px
	v, valueInk, tint, note := r.shownAt(p, k, now)
	label(img, x, y+px(24), fit(displayLabel(p, k), r.medium[22], end-x), r.medium[22], r.paint(soft))
	if !v.Valid {
		label(img, x, y+px(94), "N/A", r.faces[30], r.paint(muted))
		return
	}
	s := percentText(remainingPercent(p.ID, v.Percent))
	label(img, x, y+px(100), s, r.bold[76], valueInk)
	label(img, x+measure(r.bold[76], s)+px(1), y+px(100), "%", r.bold[34], tint)
	r.bar(img, x, y+px(118), end-x, px(8), remainingPercent(p.ID, v.Percent), tint)
	label(img, x, y+px(150), fit(note, r.faces[18], end-x), r.faces[18], r.paint(muted))
}

// Small rows (three or four windows): label and value share a line.
func (r renderer) smallRow(img draw.Image, p panel, k string, x, end, y int, now time.Time) {
	px := r.px
	v, valueInk, tint, note := r.shownAt(p, k, now)
	valueWidth := measure(r.medium[20], "N/A")
	digits := ""
	if v.Valid {
		digits = percentText(remainingPercent(p.ID, v.Percent))
		valueWidth = measure(r.bold[34], digits) + measure(r.medium[20], "%")
	}
	title := strings.TrimSpace(displayLabel(p, k))
	label(img, x, y+px(28), fit(title, r.medium[20], end-x-valueWidth-px(16)), r.medium[20], r.paint(soft))
	if !v.Valid {
		label(img, end-valueWidth, y+px(28), "N/A", r.medium[20], r.paint(muted))
		fill(img, x, y+px(41), end-x, px(1), r.paint(divider))
		return
	}
	label(img, end-valueWidth, y+px(30), digits, r.bold[34], valueInk)
	label(img, end-measure(r.medium[20], "%"), y+px(30), "%", r.medium[20], tint)
	r.bar(img, x, y+px(39), end-x, px(6), remainingPercent(p.ID, v.Percent), tint)
	label(img, x, y+px(66), fit(note, r.faces[17], end-x), r.faces[17], r.paint(muted))
}

func displayLabel(p panel, k string) string {
	name := rowLabel(p, k)
	if p.Values[k].Estimated {
		return "est. · " + name
	}
	return name
}

// The renderer owns a tightly packed RGBA image.
func rotateRGBA(img *image.RGBA) {
	for i, j := 0, len(img.Pix)-4; i < j; i, j = i+4, j-4 {
		for c := 0; c < 4; c++ {
			img.Pix[i+c], img.Pix[j+c] = img.Pix[j+c], img.Pix[i+c]
		}
	}
}
