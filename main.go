package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"
)

func main() {
	log.SetFlags(log.LstdFlags)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Print(err)
		os.Exit(1)
	}
}
func announceWeb(addr net.Addr) { log.Printf("dashboard http://%s/", addr) }
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value[:min(12, len(s.Value))]
			}
		}
	}
	return "devel"
}
func initConfig(path string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".quota-panel-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(defaultTOML)
	if err = errors.Join(err, file.Close()); err != nil {
		return err
	}
	// Link is atomic and preserves the exclusive-create contract if two
	// invocations race to initialize the same path.
	return os.Link(file.Name(), path)
}
func previewDimensions(spec string) (image.Point, error) {
	x, y, ok := strings.Cut(strings.ToLower(spec), "x")
	w, e := strconv.Atoi(x)
	h, e2 := strconv.Atoi(y)
	if !ok || e != nil || e2 != nil || w < 1 || h < 1 || w > 8192 || h > 8192 || int64(w)*int64(h) > 16<<20 {
		return image.Point{}, fmt.Errorf("invalid preview size %q", spec)
	}
	return image.Pt(w, h), nil
}
func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("trofeo-quota", flag.ContinueOnError)
	path := fs.String("config", "", "TOML settings (default: ./trofeo.toml, then embedded defaults)")
	demo := fs.Bool("demo", false, "use clearly marked sample data")
	once := fs.Bool("once", false, "collect and send one live frame")
	check := fs.Bool("doctor", false, "inspect USB, trcc, log paths and agy version")
	preview := fs.String("preview", "", "write one PNG to a file")
	sizeArg := fs.String("preview-size", "", "offline preview dimensions, e.g. 1920x462")
	page := fs.Int("page", 1, "page of quota rows for a one-shot panel or PNG (starts at 1)")
	status := fs.String("status", "", "write json, text or prometheus to stdout without accessing the LCD")
	web := fs.String("web", "", "serve dashboard on a loopback address, e.g. 127.0.0.1:9080")
	headless := fs.Bool("headless", false, "run collectors without contacting trcc")
	validate := fs.Bool("check-config", false, "validate settings and exit")
	initPath := fs.String("init-config", "", "create a new TOML settings file")
	history := fs.String("history", "", "export retained history as csv, json or jsonl to stdout")
	serviceKind := fs.String("service", "", "print a user systemd unit or launchd plist; requires -config")
	trendDays := fs.Int("trend-days", 0, "summarize recorded quotas over N UTC calendar days (1-90)")
	ver := fs.Bool("version", false, "print the build version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	trendSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "trend-days" {
			trendSet = true
		}
	})
	modes := 0
	for _, set := range []bool{trendSet, *serviceKind != "", *ver, *initPath != "", *validate, *check, *history != "", *preview != "", *status != "", *web != "", *once} {
		if set {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("choose one output mode; see -help")
	}
	if *sizeArg != "" && *preview == "" {
		return errors.New("-preview-size requires -preview")
	}
	if *once && *demo {
		return errors.New("-once reads live quotas; use -demo on its own for a sample")
	}
	canChoosePage := *preview != "" || *once || (*demo && modes == 0)
	if *page < 1 || (*page != 1 && !canChoosePage) {
		return errors.New("-page requires a positive one-shot page number")
	}
	if *demo && (trendSet || *serviceKind != "" || *check || *history != "" || *validate || *initPath != "" || *ver) {
		return errors.New("-demo applies to status, preview, web or a panel image")
	}
	if *headless && (trendSet || *serviceKind != "" || *demo || *preview != "" || *check || *once || *status != "" || *validate || *history != "" || *ver || *initPath != "") {
		return errors.New("-headless applies to the live daemon, optionally with -web")
	}
	if *ver {
		_, err := fmt.Fprintln(out, version())
		return err
	}
	if *initPath != "" {
		if *initPath == "-" {
			_, err := out.Write(defaultTOML)
			return err
		}
		return initConfig(*initPath)
	}
	c, err := readConfig(*path)
	if err != nil {
		return err
	}
	if *validate {
		_, err = fmt.Fprintln(out, "configuration OK")
		return err
	}
	if *check {
		return doctor(ctx, c)
	}
	if *serviceKind != "" {
		if *path == "" {
			return errors.New("-service requires -config")
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		return writeService(out, *serviceKind, exe, *path)
	}
	if trendSet {
		return writeTrends(ctx, out, c.Insights, time.Now(), *trendDays)
	}
	if *history != "" {
		return exportHistory(out, c.Insights, *history, time.Now())
	}
	if *status != "" && *status != "json" && *status != "text" && *status != "prometheus" {
		return errors.New("-status expects json, text or prometheus")
	}
	var size image.Point
	if *sizeArg != "" {
		size, err = previewDimensions(*sizeArg)
		if err != nil {
			return err
		}
	}
	d := newDashboard()
	var insights insights
	health := diagnostics{}
	observer := func(c config, ps, raw []panel, at time.Time) {
		d.publish(ps, at, c.Display, false)
		// History and alerts record what each quota cycle actually read, not
		// the last-good values the panel keeps showing.
		if raw == nil || (c.Insights.HistoryDir == "" && c.Insights.AlertRemaining == nil && insights.low == nil) {
			return
		}
		notices, err := insights.observe(c, statusAt(raw, at, time.Now(), false))
		health.record("insights", err)
		for _, n := range notices {
			log.Print(n)
		}
	}
	daemon := func(ctx context.Context) error { return serve(ctx, *path, c, observer, *headless) }
	if *web != "" {
		if *demo {
			daemon = func(ctx context.Context) error {
				diag := diagnostics{}
				for {
					loaded, err := c.reload(*path)
					diag.record("TOML", err)
					if err == nil {
						c = loaded
					}
					now := time.Now()
					d.publish(demoPanels(c, now), now, c.Display, true)
					timer := time.NewTimer(time.Duration(c.Interval) * time.Second)
					select {
					case <-ctx.Done():
						timer.Stop()
						return nil
					case <-timer.C:
					}
				}
			}
		}
		return serveWithWeb(ctx, *web, d, daemon)
	}
	if !*demo && !*once && *preview == "" && *status == "" {
		return daemon(ctx)
	}
	now := time.Now()
	var ps []panel
	if *demo {
		ps = demoPanels(c, now)
	} else {
		ps = diagnostics{}.snapshot(ctx, newSources(), c, panels(c))
		now = time.Now()
	}
	if *status != "" {
		return writeStatus(out, statusAt(ps, now, now, *demo), *status, c.Display.local(now).Location())
	}
	maxPage := 1
	for _, p := range ps {
		maxPage = max(maxPage, (len(p.Keys)+3)/4)
	}
	if *page > maxPage {
		return fmt.Errorf("page exceeds %d", maxPage)
	}
	frame := &scene{panels: ps, at: now, demo: *demo, page: uint64(*page - 1), display: c.Display}
	t := newTrcc(c.TrccURL)
	if *preview != "" {
		source := "explicit offline preview size"
		if *sizeArg == "" {
			size, source = t.previewSize(ctx, c.DeviceKey)
		}
		png, err := frame.pngAt(size)
		if err != nil {
			return err
		}
		log.Printf("preview %dx%d from %s", size.X, size.Y, source)
		if *preview == "-" {
			_, err = out.Write(png)
			return err
		}
		return os.WriteFile(*preview, png, 0644)
	}
	var delivery delivery
	msg, err := delivery.sync(ctx, t, c.DeviceKey, frame)
	if err == nil {
		log.Print(msg)
	}
	return err
}
