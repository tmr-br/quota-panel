package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
	"unicode"
	"unicode/utf8"
)

type serviceVariable struct{ Key, Value string }

var launchAgent = template.Must(template.New("LaunchAgent").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>io.github.tmr-br.quota-panel</string>
    <key>ProgramArguments</key>
    <array>{{range .Args}}
        <string>{{html .}}</string>{{end}}
    </array>
    <key>WorkingDirectory</key><string>{{html .Dir}}</string>
    <key>EnvironmentVariables</key>
    <dict>{{range .Env}}
        <key>{{.Key}}</key><string>{{html .Value}}</string>{{end}}
    </dict>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
</dict>
</plist>
`))

// Capture absolute search directories from the invoking shell. GUI sessions
// often lack version-manager shims and Homebrew's bin directory.
func serviceSearchPath(kind string) string {
	dirs := filepath.SplitList(os.Getenv("PATH"))
	if kind == "launchd" {
		dirs = append(dirs, "/opt/homebrew/bin")
	}
	dirs = append(dirs, "/usr/local/bin", "/usr/bin", "/bin")
	var out []string
	for _, dir := range dirs {
		if filepath.IsAbs(dir) && !slices.Contains(out, dir) {
			out = append(out, dir)
		}
	}
	return strings.Join(out, ":")
}

func writeService(w io.Writer, kind, exe, configPath string) error {
	if kind != "systemd" && kind != "launchd" {
		return errors.New("-service expects systemd or launchd")
	}
	if exe == "" || configPath == "" {
		return errors.New("service requires executable and config paths")
	}
	binary, err := filepath.Abs(exe)
	if err != nil {
		return err
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	args := []string{binary, "-config", configPath}

	env := []serviceVariable{{"PATH", serviceSearchPath(kind)}}
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		if value := os.Getenv(key); value != "" {
			abs, err := filepath.Abs(value)
			if err != nil {
				return err
			}
			env = append(env, serviceVariable{key, abs})
		}
	}
	paths := []string{binary, dir, configPath}
	for _, v := range env {
		paths = append(paths, v.Value)
	}
	for _, s := range paths {
		if !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0 || strings.ContainsAny(s, "\ufffe\uffff") {
			return errors.New("service paths must be valid UTF-8 without control characters or invalid XML characters")
		}
	}
	if kind == "launchd" {
		return launchAgent.Execute(w, struct {
			Args []string
			Dir  string
			Env  []serviceVariable
		}{args, dir, env})
	}

	// The ':' prefix disables dollar expansion. Specifiers still expand, and
	// quoted words use systemd's C escapes. The fixed shell command only
	// forwards positional arguments: systemd rejects quotes in the executable
	// path, and env would treat a path containing '=' as an assignment.
	escape := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%")
	quote := func(word string) string { return `"` + escape.Replace(word) + `"` }
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = quote(arg)
	}
	envLines := make([]string, len(env))
	for i, v := range env {
		envLines[i] = "Environment=" + quote(v.Key+"="+v.Value)
	}
	// WorkingDirectory= takes one raw path, with specifier expansion and no
	// quote removal. A trailing slash preserves final whitespace or backslash.
	dir = strings.ReplaceAll(strings.TrimSuffix(dir, "/")+"/", "%", "%%")
	_, err = fmt.Fprintf(w, `[Unit]
Description=Trofeo quota panel

[Service]
WorkingDirectory=%s
%s
ExecStart=:/bin/sh -c 'exec "$@"' quota-panel %s
Restart=on-failure
RestartSec=10s

[Install]
WantedBy=default.target
`, dir, strings.Join(envLines, "\n"), strings.Join(quoted, " "))
	return err
}
