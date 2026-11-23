package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"
)

func main() {
	log.SetFlags(log.LstdFlags)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	path := flag.String("config", "", "TOML settings (default: ./trofeo.toml, then embedded defaults)")
	demo := flag.Bool("demo", false, "send one clearly marked sample image")
	once := flag.Bool("once", false, "collect and send one live frame, then exit")
	check := flag.Bool("doctor", false, "inspect USB, trcc, log paths and agy version")
	preview := flag.String("preview", "", "write one PNG using detected dimensions or the offline 9.16-inch catalog size")
	flag.Parse()
	c, err := readConfig(*path)
	if err != nil {
		return err
	}
	if *check {
		return doctor(ctx, c)
	}
	if !*demo && !*once && *preview == "" {
		return serve(ctx, *path, c)
	}
	now := time.Now()
	var ps []panel
	if *demo {
		ps = demoPanels(c, now)
	} else {
		ps = diagnostics{}.snapshot(ctx, newSources(), c)
		now = time.Now()
	}
	frame := &scene{panels: ps, at: now, demo: *demo}
	t := newTrcc(c.TrccURL)
	if *preview != "" {
		size, source := t.previewSize(ctx, c.DeviceKey)
		png, err := frame.pngAt(size)
		if err != nil {
			return err
		}
		log.Printf("preview %dx%d from %s", size.X, size.Y, source)
		return os.WriteFile(*preview, png, 0644)
	}
	var d delivery
	msg, err := d.sync(ctx, t, c.DeviceKey, frame)
	if err == nil {
		log.Print(msg)
	}
	return err
}
