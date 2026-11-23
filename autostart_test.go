package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLaunchAgentArgumentsAndEnvironment(t *testing.T) {
	t.Setenv("PATH", "/tools/bin:.:/space & quote'/bin:/tools/bin")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "accounts/Work & home")
	const exe = `quoted "LCD" $HOME & %n/trofeo-quota`
	const configPath = `format '<&>/quota.toml`
	var b bytes.Buffer
	if err := writeService(&b, "launchd", exe, configPath); err != nil {
		t.Fatal(err)
	}
	var plist struct {
		Args   []string `xml:"dict>array>string"`
		Fields []string `xml:"dict>string"`
		Env    []string `xml:"dict>dict>string"`
	}
	if err := xml.Unmarshal(b.Bytes(), &plist); err != nil {
		t.Fatal(err)
	}
	absExe, err := filepath.Abs(exe)
	if err != nil {
		t.Fatal(err)
	}
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plist.Args, []string{absExe, "-config", absConfig}) || !slices.Contains(plist.Fields, cwd) {
		t.Fatalf("unexpected launch arguments or directory: %+v", plist)
	}
	if !slices.Equal(plist.Env, []string{"/tools/bin:/space & quote'/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin", filepath.Join(cwd, "accounts/Work & home")}) {
		t.Fatalf("unexpected launch PATH: %q", plist.Env)
	}
}

func TestSystemdUsesLiteralPaths(t *testing.T) {
	t.Setenv("PATH", `relative:/node %n/quote"'/bin:/usr/bin`)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", `/claude $d/%h/a"b`)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	const exe = `/app=prod/$HOME %n/'"\tool`
	const configPath = `/settings $var "quoted" %u\/dashboard.toml`
	var b bytes.Buffer
	if err := writeService(&b, "systemd", exe, configPath); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`ExecStart=:/bin/sh -c 'exec "$@"' quota-panel "/app=prod/$HOME %%n/'\"\\tool" "-config" "/settings $var \"quoted\" %%u\\/dashboard.toml"`,
		"WorkingDirectory=" + strings.ReplaceAll(strings.TrimSuffix(cwd, "/")+"/", "%", "%%"),
		`Environment="CLAUDE_CONFIG_DIR=/claude $d/%%h/a\"b"`,
		`Environment="PATH=/node %%n/quote\"'/bin:/usr/bin:/usr/local/bin:/bin"`,
		`WantedBy=default.target`,
	} {
		if !strings.Contains(b.String(), line+"\n") {
			t.Errorf("missing line %q in:\n%s", line, b.String())
		}
	}
}

func TestServiceRejectsInvalidInputAndReportsWriteErrors(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	for _, tc := range [][3]string{
		{"other", "/tool", "/config"}, {"launchd", "", "/config"}, {"systemd", "/tool", ""},
		{"systemd", "/tool", "/bad\npath"}, {"launchd", "/tool", "/bad\xff"},
	} {
		var b bytes.Buffer
		if err := writeService(&b, tc[0], tc[1], tc[2]); err == nil || b.Len() != 0 {
			t.Errorf("invalid service options %q produced output", tc)
		}
	}
	for _, kind := range []string{"launchd", "systemd"} {
		reader, writer := io.Pipe()
		reader.Close()
		err := writeService(writer, kind, "/tool", "/config")
		writer.Close()
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("%s lost output error: %v", kind, err)
		}
	}
}
