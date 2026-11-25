package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type observedFrame struct {
	panels  []panel
	at      time.Time
	display displayConfig
	demo    bool
}
type dashboard struct {
	current atomic.Pointer[observedFrame]
	render  chan struct{}
}

func newDashboard() *dashboard { return &dashboard{render: make(chan struct{}, 1)} }
func (d *dashboard) publish(ps []panel, at time.Time, c displayConfig, demo bool) {
	d.current.Store(&observedFrame{ps, at, c, demo})
}

func loopbackAddress(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("web address must be loopback host:port: %w", err)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	n, err := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || err != nil || n < 0 || n > 65535 {
		return "", fmt.Errorf("web address must use a loopback IP and numeric port")
	}
	return net.JoinHostPort(host, port), nil
}

//go:embed assets/dashboard.html
var dashboardHTML string

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"percentage": func(v *float64) string {
		if v == nil {
			return "N/A"
		}
		return percentText(*v) + "%"
	},
	"width": func(v *float64) float64 {
		if v == nil {
			return 0
		}
		return min(100, max(0, *v))
	},
	"known": func(v *float64) bool { return v != nil },
	"date":  func(t time.Time, loc *time.Location) string { return t.In(loc).Format("Jan 2 15:04 MST") },
}).Parse(dashboardHTML))

func (d *dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	host := r.Host
	if host == "localhost" || net.ParseIP(host) != nil || (strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") && net.ParseIP(host[1:len(host)-1]) != nil) {
		host = net.JoinHostPort(strings.Trim(host, "[]"), "80")
	}
	if _, err := loopbackAddress(host); err != nil {
		http.Error(w, "local host required", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "read only", 405)
		return
	}
	path := r.URL.Path
	switch path {
	case "/", "/api/status", "/metrics", "/healthz", "/panel.png":
	default:
		http.NotFound(w, r)
		return
	}
	f := d.current.Load()
	if f == nil {
		w.Header().Set("Retry-After", "2")
		if path == "/" {
			w.Header().Set("Refresh", "2")
		}
		http.Error(w, "Loading...", 503)
		return
	}
	now := time.Now()
	if path == "/panel.png" {
		page := uint64(1)
		if raw := r.URL.Query().Get("page"); raw != "" {
			n, err := strconv.ParseUint(raw, 10, 32)
			maxPage := 1
			for _, p := range f.panels {
				maxPage = max(maxPage, (len(p.Keys)+3)/4)
			}
			if err != nil || n == 0 || n > uint64(maxPage) {
				http.Error(w, "invalid page", 400)
				return
			}
			page = n
		}
		select {
		case d.render <- struct{}{}:
			defer func() { <-d.render }()
		case <-r.Context().Done():
			return
		}
		s := scene{panels: f.panels, at: now, display: f.display, demo: f.demo, page: page - 1}
		png, err := s.pngAt(catalogSize())
		if err != nil {
			http.Error(w, "render failed", 500)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
		return
	}
	s := statusAt(f.panels, f.at, now, f.demo)
	switch path {
	case "/api/status":
		w.Header().Set("Content-Type", "application/json")
		_ = writeStatus(w, s, "json")
	case "/metrics":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = writeStatus(w, s, "prometheus")
	case "/healthz":
		w.Header().Set("Content-Type", "application/json")
		available, total := 0, 0
		for _, g := range s.Groups {
			for _, row := range g.Rows {
				total++
				if row.Percent != nil {
					available++
				}
			}
		}
		fmt.Fprintf(w, `{"ready":true,"available":%d,"total":%d,"demo":%t}`, available, total, s.Demo)
	case "/":
		// Refresh the document when the earliest known reading expires, even
		// between provider polls. A slow provider never prolongs its validity here.
		wait := 15
		if expiry := nextExpiration(f.panels, now); !expiry.IsZero() {
			wait = min(wait, max(1, int(time.Until(expiry).Seconds())+1))
		}
		w.Header().Set("Refresh", strconv.Itoa(wait))
		loc := f.display.local(now).Location()
		var buf bytes.Buffer
		if err := dashboardTemplate.Execute(&buf, struct {
			Snap quotaSnapshot
			Loc  *time.Location
		}{s, loc}); err != nil {
			http.Error(w, "template error", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(buf.Bytes())
	}
}
func serveWithWeb(ctx context.Context, addr string, d *dashboard, collect func(context.Context) error) error {
	normalized, err := loopbackAddress(addr)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", normalized)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{Handler: d, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 45 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ln); cancel() }()
	announceWeb(ln.Addr())
	err = collect(ctx)
	cancel()
	closeErr := server.Close()
	webErr := <-done
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if webErr != nil && webErr != http.ErrServerClosed {
		return webErr
	}
	return nil
}
