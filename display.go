package main

import (
	"fmt"
	"time"
)

type displayConfig struct {
	Timezone     string `toml:"timezone"`
	HighContrast bool   `toml:"high_contrast"`
	Rotate180    bool   `toml:"rotate_180"`
	PageSeconds  int    `toml:"page_seconds"`
	location     *time.Location
}

func (d *displayConfig) validate() error {
	if d.PageSeconds < 0 || d.PageSeconds > 86400 {
		return fmt.Errorf("display.page_seconds must be between 0 and 86400")
	}
	if d.Timezone != "" {
		loc, err := time.LoadLocation(d.Timezone)
		if err != nil {
			return fmt.Errorf("display.timezone: %w", err)
		}
		d.location = loc
	}
	return nil
}
func (d displayConfig) local(t time.Time) time.Time {
	if d.location != nil {
		return t.In(d.location)
	}
	return t.Local()
}
