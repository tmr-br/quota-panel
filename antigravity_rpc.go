package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type localEndpoint struct {
	port          int
	scheme, token string
}

func languageServerName(path string) bool {
	n := strings.ReplaceAll(strings.ToLower(filepath.Base(path)), "-", "_")
	return n == "language_server" || strings.HasPrefix(n, "language_server_")
}

// First enumerate only executable names for the current user. Read argv only
// for matching language servers. Never put argv or tokens in errors/logs.
func languageEndpoints(ctx context.Context) ([]localEndpoint, error) {
	uid := strconv.Itoa(os.Getuid())
	b, err := commandOutput(ctx, 4*time.Second, "ps", "-ww", "-U", uid, "-o", "pid=,comm=")
	if err != nil {
		return nil, err
	}
	var endpoints []localEndpoint
	var failures []error
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !languageServerName(strings.Join(fields[1:], " ")) {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		args, err := commandOutput(ctx, 3*time.Second, "ps", "-ww", "-p", fields[0], "-o", "uid=,args=")
		if err != nil {
			failures = append(failures, err)
			continue
		}
		words := strings.Fields(string(args))
		if len(words) < 2 || words[0] != uid {
			continue
		}
		endpoints = append(endpoints, endpointsFromArgs(words[1:])...)
	}
	if len(endpoints) == 0 {
		return nil, errors.Join(append(failures, errors.New("no running Antigravity language server with a local port and CSRF token in argv"))...)
	}
	return endpoints, nil
}

func endpointsFromArgs(args []string) []localEndpoint {
	identity := strings.ToLower(strings.Join(args, " "))
	if !strings.Contains(identity, "antigravity") && !strings.Contains(identity, "/gemini.app/") {
		return nil
	}
	flags := map[string]string{}
	for i, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		k, v, found := strings.Cut(arg, "=")
		if !found && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			v = args[i+1]
		}
		flags[strings.ReplaceAll(k[2:], "-", "_")] = v
	}
	var out []localEndpoint
	for _, candidate := range []struct {
		key     string
		schemes []string
	}{
		{"https_port", []string{"https"}}, {"http_port", []string{"http"}},
		{"language_server_port", []string{"https", "http"}}, {"server_port", []string{"https", "http"}},
		{"port", []string{"https", "http"}}, {"extension_server_port", []string{"http", "https"}},
	} {
		port, err := strconv.Atoi(flags[candidate.key])
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		token := flags["csrf_token"]
		if candidate.key == "extension_server_port" && flags["extension_server_csrf_token"] != "" {
			token = flags["extension_server_csrf_token"]
		}
		if token == "" {
			continue
		}
		for _, scheme := range candidate.schemes {
			ep := localEndpoint{port, scheme, token}
			if !slices.Contains(out, ep) {
				out = append(out, ep)
			}
		}
	}
	return out
}

const rpcPrefix = "/exa.language_server_pb.LanguageServerService/"

func rpcCall(ctx context.Context, client *http.Client, ep localEndpoint, method, body string) ([]byte, error) {
	target := fmt.Sprintf("%s://127.0.0.1:%d%s%s", ep.scheme, ep.port, rpcPrefix, method)
	req, err := http.NewRequestWithContext(ctx, "POST", target, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("X-Codeium-Csrf-Token", ep.token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: HTTP %d", method, resp.StatusCode)
	}
	const max = 2 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > max {
		return nil, errors.New("RPC response too large")
	}
	return raw, nil
}

type rpcDocument struct {
	agSummary
	Code                            json.RawMessage
	Response, Summary, QuotaSummary *agSummary
	UserStatus                      struct {
		QuotaSummary, UserQuotaSummary *agSummary
		CascadeModelConfigData         struct {
			ClientModelConfigs []struct {
				Label     string
				QuotaInfo struct {
					agBucket
					Window string
				}
			}
		}
	}
}

func rpcReadings(raw []byte, keys []string, now time.Time) (readings, error) {
	var doc rpcDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	code := string(bytes.TrimSpace(doc.Code))
	if code != "" && code != "null" && code != "0" && code != `"OK"` && code != `"0"` {
		return nil, errors.New("language server returned an RPC error")
	}
	for _, s := range []*agSummary{doc.Response, doc.Summary, doc.QuotaSummary, doc.UserStatus.QuotaSummary, doc.UserStatus.UserQuotaSummary, &doc.agSummary} {
		if s != nil && len(s.Groups) > 0 {
			return s.readings(keys, now)
		}
	}
	// Early GetUserStatus implementations expose individual models. A
	// fraction and reset time alone cannot distinguish a weekly from a 5h
	// bucket. Accept only explicit bucket ids or a stated pool and cadence.
	out := readings{}
	for _, m := range doc.UserStatus.CascadeModelConfigData.ClientModelConfigs {
		b := m.QuotaInfo
		key := bucketKey[b.BucketID]
		if key == "" {
			key = bucketKey[b.ID]
		}
		if key == "" {
			var group, window string
			switch strings.ToLower(m.Label) {
			case "gemini models", "gemini":
				group = "gemini"
			case "claude and gpt models", "claude / gpt", "claude/gpt":
				group = "claude_gpt"
			}
			switch strings.ToLower(b.Window) {
			case "5h", "five_hour":
				window = "5h"
			case "weekly", "week":
				window = "week"
			}
			if group == "" || window == "" {
				continue
			}
			key = group + "." + window
		}
		r := b.read(now)
		if prev, ok := out[key]; ok && prev != r {
			r = reading{}
		}
		out[key] = r
	}
	return out, missingReadings(keys, out)
}

func fetchRPC(ctx context.Context, keys []string, now time.Time) (readings, error) {
	ctx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	endpoints, err := languageEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	return queryEndpoints(ctx, endpoints, keys, now)
}

func queryEndpoints(ctx context.Context, endpoints []localEndpoint, keys []string, now time.Time) (readings, error) {
	// Antigravity issues a self-signed certificate on its local HTTPS port.
	// This transport has no proxy, never redirects and is only used with the
	// literal loopback address and a port advertised by the user's server.
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var last error
	var best readings
	bestCount := 0
	for _, ep := range endpoints {
		chosen := readings{}
		for _, op := range []struct{ name, body string }{
			{"RetrieveUserQuotaSummary", `{"forceRefresh":true}`},
			{"GetUserStatus", `{"metadata":{"ideName":"antigravity","extensionName":"antigravity","locale":"en"}}`},
		} {
			if err := ctx.Err(); err != nil {
				return best, errors.Join(err, missingReadings(keys, best))
			}
			b, err := rpcCall(ctx, client, ep, op.name, op.body)
			if err != nil {
				last = err
				continue
			}
			values, err := rpcReadings(b, keys, now)
			for k, v := range values {
				if !chosen[k].Valid && v.Valid {
					chosen[k] = v
				}
			}
			if missingReadings(keys, chosen) == nil {
				return chosen, nil
			}
			last = err
			count := 0
			for _, k := range keys {
				if chosen[k].Valid {
					count++
				}
			}
			if count > bestCount {
				bestCount = count
				best = chosen
			}
		}
	}
	return best, errors.Join(last, missingReadings(keys, best))
}
