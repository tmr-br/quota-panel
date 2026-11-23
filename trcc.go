package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type restReply struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}
type product struct {
	Key    string `json:"key"`
	VID    int    `json:"vid"`
	PID    int    `json:"pid"`
	Wire   string `json:"wire"`
	Native []int  `json:"native_resolution"`
}
type deviceState struct {
	restReply
	Connected  bool  `json:"connected"`
	Resolution []int `json:"resolution"`
}

func (s deviceState) canvas() (image.Point, error) {
	if !s.Connected || len(s.Resolution) != 2 || s.Resolution[0] <= 0 || s.Resolution[1] <= 0 {
		return image.Point{}, errors.New("live handshake resolution unavailable")
	}
	return image.Pt(s.Resolution[0], s.Resolution[1]), nil
}

// The preview has no side effects on trcc. Prefer a live handshake if one is
// already available; the offline catalog value is distinct in diagnostics.
func (t trcc) previewSize(ctx context.Context, key string) (image.Point, string) {
	p, err := t.discover(ctx, key)
	if err == nil {
		var s deviceState
		s, err = t.state(ctx, p)
		if err == nil {
			var size image.Point
			size, err = s.canvas()
			if err == nil {
				return size, "trcc handshake " + p.Key
			}
		}
		if len(p.Native) == 2 && p.Native[0] > 0 && p.Native[1] > 0 {
			return image.Pt(p.Native[0], p.Native[1]), fmt.Sprintf("trcc catalog %s (%v)", p.Key, err)
		}
	}
	return catalogSize(), fmt.Sprintf("offline Trofeo 9.16 catalog (%v)", err)
}

type trcc struct {
	base   string
	client *http.Client
}

func newTrcc(base string) trcc {
	return trcc{base, &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type httpFailure struct {
	status int
	path   string
}

func (e httpFailure) Error() string { return fmt.Sprintf("trcc %s: HTTP %d", e.path, e.status) }

func (t trcc) request(ctx context.Context, method, path, kind string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, t.base+path, body)
	if err != nil {
		return err
	}
	if kind != "" {
		req.Header.Set("Content-Type", kind)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpFailure{resp.StatusCode, path}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var status restReply
	if err := json.Unmarshal(b, &status); err != nil {
		return fmt.Errorf("trcc %s: %w", path, err)
	}
	if !status.OK {
		return fmt.Errorf("trcc %s: ok was not true (%s)", path, status.Message)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (t trcc) discover(ctx context.Context, wanted string) (product, error) {
	var list struct {
		Products []product `json:"products"`
	}
	if err := t.request(ctx, "GET", "/devices", "", nil, &list); err != nil {
		return product{}, err
	}
	var found []product
	for _, p := range list.Products {
		key, _, _ := strings.Cut(strings.ToLower(p.Key), "@")
		if key != "0416:5408" && key != "0416:5409" {
			continue
		}
		if p.VID != 0 && p.VID != 0x416 || p.PID != 0 && fmt.Sprintf("0416:%04x", p.PID) != key {
			continue
		}
		if wanted == "" || strings.EqualFold(wanted, p.Key) {
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		return product{}, errors.New("trcc did not discover a matching 0416:5408/5409 panel")
	}
	if len(found) > 1 {
		return product{}, errors.New("multiple Trofeo panels: set device_key in TOML")
	}
	return found[0], nil
}

func devicePath(key string) string { return "/devices/" + url.PathEscape(key) }
func (t trcc) state(ctx context.Context, p product) (deviceState, error) {
	var state deviceState
	err := t.request(ctx, "GET", devicePath(p.Key)+"/state", "", nil, &state)
	return state, err
}
func (t trcc) upload(ctx context.Context, p product, png []byte) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("image", "quota.png")
	if err != nil {
		return err
	}
	if _, err = part.Write(png); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	return t.request(ctx, "POST", devicePath(p.Key)+"/display/send-image", w.FormDataContentType(), &body, nil)
}

type delivery struct {
	target       string
	digest       [sha256.Size]byte
	acknowledged bool
}

// An observed disconnect invalidates the acknowledgement even when the PNG is
// unchanged. Failed uploads are never acknowledged.
func (d *delivery) sync(ctx context.Context, t trcc, wanted string, frame *scene) (string, error) {
	p, err := t.discover(ctx, wanted)
	if err != nil {
		d.acknowledged = false
		return "", err
	}
	state, err := t.state(ctx, p)
	var httpErr httpFailure
	absent := errors.As(err, &httpErr) && httpErr.status == 404
	if err != nil && !absent {
		d.acknowledged = false
		return "", err
	}
	if !state.Connected {
		d.acknowledged = false
		if err = t.request(ctx, "POST", devicePath(p.Key)+"/connect", "", nil, nil); err != nil {
			return "", err
		}
		state, err = t.state(ctx, p)
		if err != nil {
			return "", err
		}
		if !state.Connected {
			return "", errors.New("trcc handshake has not connected the LCD")
		}
	}
	size, err := state.canvas()
	if err != nil {
		d.acknowledged = false
		return "", fmt.Errorf("%s: %w (catalog %v)", p.Key, err, p.Native)
	}
	png, err := frame.pngAt(size)
	if err != nil {
		d.acknowledged = false
		return "", err
	}
	target := fmt.Sprintf("%s/%s %dx%d", t.base, p.Key, size.X, size.Y)
	digest := sha256.Sum256(png)
	if d.acknowledged && d.target == target && d.digest == digest {
		return "", nil
	}
	if err := t.upload(ctx, p, png); err != nil {
		d.acknowledged = false
		return "", err
	}
	*d = delivery{target, digest, true}
	return "trcc accepted native PNG: " + target, nil
}
