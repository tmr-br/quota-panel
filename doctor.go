package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

func thermalrightUSB(ctx context.Context) ([]string, error) {
	var ids []string
	switch runtime.GOOS {
	case "linux":
		dir := "/sys/bus/usb/devices"
		devices, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, device := range devices {
			p := filepath.Join(dir, device.Name())
			v, err := os.ReadFile(filepath.Join(p, "idVendor"))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(string(v)) != "0416" {
				continue
			}
			product, err := os.ReadFile(filepath.Join(p, "idProduct"))
			if err != nil {
				return nil, err
			}
			ids = append(ids, "0416:"+strings.TrimSpace(string(product)))
		}
	case "darwin":
		return macUSB(ctx)
	default:
		return nil, fmt.Errorf("USB inventory unavailable for %s", runtime.GOOS)
	}
	return ids, nil
}

// Unsupported data types can produce successful, empty reports. Select a
// type that the host actually advertises before interpreting an empty list.
func macUSB(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	list, err := commandOutput(ctx, 20*time.Second, "system_profiler", "-listDataTypes")
	if err != nil {
		return nil, err
	}
	types := strings.Fields(string(list))
	report := "SPUSBHostDataType"
	if !slices.Contains(types, report) {
		report = "SPUSBDataType"
		if !slices.Contains(types, report) {
			return nil, errors.New("system_profiler lists no supported USB report")
		}
	}
	b, err := commandOutput(ctx, 20*time.Second, "system_profiler", report, "-json")
	if err != nil {
		return nil, err
	}
	type node struct {
		Vendor      string `json:"vendor_id"`
		Product     string `json:"product_id"`
		HostVendor  string `json:"USBDeviceKeyVendorID"`
		HostProduct string `json:"USBDeviceKeyProductID"`
		Children    []node `json:"_items"`
	}
	var tree map[string][]node
	if err := json.Unmarshal(b, &tree); err != nil {
		return nil, err
	}
	nodes := tree[report]
	if nodes == nil {
		return nil, fmt.Errorf("system_profiler response lacks %s device list", report)
	}
	var ids []string
	for len(nodes) > 0 {
		n := nodes[len(nodes)-1]
		nodes = append(nodes[:len(nodes)-1], n.Children...)
		vendor, product := n.Vendor, n.Product
		if report == "SPUSBHostDataType" {
			vendor, product = n.HostVendor, n.HostProduct
		}
		v := strings.Fields(strings.ToLower(vendor))
		if len(v) == 0 || v[0] != "0x0416" {
			continue
		}
		p := strings.Fields(strings.ToLower(product))
		if len(p) == 0 {
			return nil, errors.New("system_profiler omitted the product ID of a 0416 device")
		}
		pid, err := strconv.ParseUint(strings.TrimPrefix(p[0], "0x"), 16, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid 0416 product ID: %w", err)
		}
		ids = append(ids, fmt.Sprintf("0416:%04x", pid))
	}
	return ids, nil
}

func doctor(ctx context.Context, c config) error {
	fmt.Printf("Host %s/%s, offline catalog %dx%d; upload dimensions come from the handshake\n", runtime.GOOS, runtime.GOARCH, catalogWidth, catalogHeight)
	ids, err := thermalrightUSB(ctx)
	switch {
	case err != nil:
		fmt.Println("USB inventory unavailable:", err)
	case len(ids) == 0:
		fmt.Println("USB: no visible 0416 devices")
	default:
		fmt.Println("Observed USB IDs:", strings.Join(ids, ", "))
	}
	for _, bin := range []string{"trcc", "npx"} {
		if path, err := exec.LookPath(bin); err != nil {
			fmt.Printf("%s: unavailable\n", bin)
		} else {
			fmt.Printf("%s: %s\n", bin, path)
		}
	}
	for _, source := range []struct{ label, override, env, base, leaf string }{
		{"Claude", c.Claude.ProjectsDir, "CLAUDE_CONFIG_DIR", ".claude", "projects"},
		{"Codex", c.Codex.SessionsDir, "CODEX_HOME", ".codex", "sessions"},
	} {
		path, err := logPath(source.override, source.env, source.base, source.leaf)
		count := 0
		if err == nil {
			var dir string
			dir, err = filepath.EvalSymlinks(path)
			if err == nil {
				err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, e error) error {
					if e != nil {
						return e
					}
					if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".jsonl") {
						count++
					}
					return ctx.Err()
				})
			}
		}
		if err != nil {
			fmt.Printf("%s %s: unavailable (%v)\n", source.label, path, err)
		} else {
			fmt.Printf("%s %s: %d JSONL files\n", source.label, path, count)
		}
	}
	tool, err := inspectAgy(ctx)
	if err != nil {
		fmt.Printf("agy --version: unavailable (%v)\n", err)
	} else {
		fmt.Printf("agy --version: %s\n", tool.version)
	}
	if err != nil || semver.Compare(tool.version, "v1.1.11") < 0 {
		endpoints, e := languageEndpoints(ctx)
		if e != nil {
			fmt.Println("Antigravity Connect-RPC:", e)
		} else {
			fmt.Printf("Antigravity Connect-RPC: %d local endpoint candidates\n", len(endpoints))
		}
	}
	t := newTrcc(c.TrccURL)
	fmt.Printf("trcc %s; POST /devices/{key}/display/send-image (multipart image)\n", c.TrccURL)
	p, err := t.discover(ctx, c.DeviceKey)
	if err != nil {
		return err
	}
	fmt.Printf("Discovered %s, wire %s, catalog pixels %v\n", p.Key, p.Wire, p.Native)
	s, err := t.state(ctx, p)
	if err != nil {
		return fmt.Errorf("handshake unavailable: %w", err)
	}
	size, err := s.canvas()
	if err != nil {
		return fmt.Errorf("handshake %s: %w", p.Key, err)
	}
	fmt.Printf("Live handshake %s: %dx%d\n", p.Key, size.X, size.Y)
	return nil
}
