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
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const catalogWidth, catalogHeight = 1920, 462

func catalogSize() image.Point { return image.Pt(catalogWidth, catalogHeight) }

// A snapshot is rendered directly at the active panel's native resolution.
// Cache its PNG across health probes; a different handshake needs new pixels.
type scene struct {
	panels []panel
	at     time.Time
	demo   bool
	page   uint64
	size   image.Point
	png    []byte
}

func (s *scene) pngAt(size image.Point) ([]byte, error) {
	if size == s.size && s.png != nil {
		return s.png, nil
	}
	r, err := newRenderer(size)
	if err != nil {
		return nil, err
	}
	b, err := r.frame(s.panels, s.at, s.demo, s.page)
	if err != nil {
		return nil, err
	}
	s.size, s.png = size, b
	return b, nil
}

//go:embed assets/panel.otf
var japaneseFont []byte

type renderer struct {
	size  image.Point
	scale float64
	faces map[int]font.Face
	bold  map[int]font.Face
}

func newRenderer(size image.Point) (renderer, error) {
	r := renderer{size: size, scale: min(float64(size.X)/catalogWidth, float64(size.Y)/catalogHeight), faces: map[int]font.Face{}, bold: map[int]font.Face{}}
	if size.X <= 0 || size.Y <= 0 || size.X > 8192 || size.Y > 8192 || int64(size.X)*int64(size.Y) > 16<<20 {
		return r, fmt.Errorf("invalid trcc canvas %dx%d", size.X, size.Y)
	}
	for _, set := range []struct {
		data  []byte
		sizes []int
		faces map[int]font.Face
	}{
		{japaneseFont, []int{18, 20, 22, 25, 36}, r.faces},
		{gobold.TTF, []int{16, 32, 39, 42, 70}, r.bold},
	} {
		f, err := opentype.Parse(set.data)
		if err != nil {
			return r, err
		}
		for _, size := range set.sizes {
			face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: float64(size) * r.scale, DPI: 72, Hinting: font.HintingFull})
			if err != nil {
				return r, err
			}
			set.faces[size] = face
		}
	}
	return r, nil
}
func (r renderer) px(n int) int { return max(1, int(math.Round(float64(n)*r.scale))) }

func rgb(v uint32) color.RGBA { return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255} }

var (
	background = rgb(0x10171e)
	divider    = rgb(0x30404a)
	track      = rgb(0x2b3944)
	ink        = rgb(0xf4f7f8)
	muted      = rgb(0xa7b6c2)
	accents    = map[string]color.RGBA{"claude": rgb(0xf2a183), "codex": rgb(0x71d6bd), "antigravity": rgb(0xb1a6f8), "opencode": rgb(0x86c5fa)}
)

func fill(img draw.Image, x, y, w, h int, c color.Color) {
	draw.Draw(img, image.Rect(x, y, x+w, y+h), image.NewUniform(c), image.Point{}, draw.Src)
}
func label(img draw.Image, x, baseline int, s string, face font.Face, c color.Color) {
	d := font.Drawer{Dst: img, Src: image.NewUniform(c), Face: face, Dot: fixed.P(x, baseline)}
	d.DrawString(s)
}
func fit(s string, f font.Face, max int) string {
	if font.MeasureString(f, s).Ceil() <= max {
		return s
	}
	runes := []rune(s)
	for len(runes) > 0 {
		runes = runes[:len(runes)-1]
		t := string(runes) + "…"
		if font.MeasureString(f, t).Ceil() <= max {
			return t
		}
	}
	return "…"
}

func (r renderer) frame(ps []panel, now time.Time, demo bool, page uint64) ([]byte, error) {
	width, height := r.size.X, r.size.Y
	px := r.px
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	fill(img, 0, 0, width, height, background)
	if len(ps) == 0 {
		label(img, px(48), height/2+px(14), "表示対象がありません", r.faces[36], muted)
	}
	for i, p := range ps {
		left, right := i*width/len(ps), (i+1)*width/len(ps)
		if i > 0 {
			fill(img, left, px(20), px(2), height-2*px(20), divider)
		}
		x, end := left+px(28), right-px(28)
		accent := accents[p.ID]
		fill(img, x, px(37), px(24), px(4), accent)
		label(img, x+px(36), px(45), p.Unit, r.faces[20], accent)
		if demo {
			label(img, end-font.MeasureString(r.bold[16], "DEMO").Ceil(), px(44), "DEMO", r.bold[16], muted)
		}
		label(img, x, px(100), p.Title, r.bold[42], ink)
		fill(img, x, px(116), end-x, px(2), divider)
		pages := (len(p.Keys) + 3) / 4
		index := int(page % uint64(pages))
		keys := p.Keys[index*4 : min((index+1)*4, len(p.Keys))]
		if pages > 1 {
			s := fmt.Sprintf("%d/%d", index+1, pages)
			label(img, end-font.MeasureString(r.bold[16], s).Ceil(), px(96), s, r.bold[16], muted)
		}
		if len(p.Keys) <= 2 {
			for j, k := range keys {
				r.largeRow(img, p, k, x, end, px(127)+(height-px(141))*j/2, now)
			}
		} else {
			for j, k := range keys {
				r.smallRow(img, p, k, x, end, px(125)+(height-px(136))*j/4, now)
			}
		}
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
	if v > 0 && v < 1 {
		return "<1"
	}
	if v > 99 && v < 100 {
		return ">99"
	}
	return fmt.Sprintf("%.0f", v)
}

func bar(img draw.Image, x, y, w, h int, ratio float64, c color.Color) {
	fill(img, x, y, w, h, track)
	n := int(math.Round(float64(w) * min(ratio, 100) / 100))
	if ratio > 0 && n == 0 {
		n = 1
	}
	fill(img, x, y, n, h, c)
}

func (r renderer) largeRow(img draw.Image, p panel, k string, x, end, y int, now time.Time) {
	px := r.px
	v := p.valueAt(k, now)
	label(img, x, y+px(30), fit(rowLabel(p, k), r.faces[25], end-x), r.faces[25], ink)
	if !v.Valid {
		label(img, x, y+px(98), "取得不能", r.faces[36], muted)
		return
	}
	s := percentText(v.Percent)
	label(img, x, y+px(99), s, r.bold[70], ink)
	label(img, x+font.MeasureString(r.bold[70], s).Ceil()+px(6), y+px(97), "%", r.bold[32], accents[p.ID])
	bar(img, x, y+px(112), end-x, px(8), v.Percent, accents[p.ID])
	label(img, x, y+px(146), resetText(p.ID, v, now), r.faces[20], muted)
}

func (r renderer) smallRow(img draw.Image, p panel, k string, x, end, y int, now time.Time) {
	px := r.px
	v := p.valueAt(k, now)
	value := "取得不能"
	face := r.faces[22]
	c := muted
	if v.Valid {
		value = percentText(v.Percent) + "%"
		face = r.bold[39]
		c = ink
	}
	valueWidth := font.MeasureString(face, value).Ceil()
	title := strings.TrimSpace(rowLabel(p, k))
	label(img, x, y+px(34), fit(title, r.faces[22], end-x-valueWidth-px(12)), r.faces[22], ink)
	label(img, end-valueWidth, y+px(34), value, face, c)
	if !v.Valid {
		fill(img, x, y+px(60), end-x, px(2), divider)
		return
	}
	bar(img, x, y+px(43), end-x, px(6), v.Percent, accents[p.ID])
	label(img, x, y+px(72), resetText(p.ID, v, now), r.faces[18], muted)
}
