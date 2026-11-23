package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type localEndpoint struct {
	pid           int
	addr          netip.AddrPort
	scheme, token string
	advertised    bool
}

func languageServerName(path string) bool {
	n := strings.ReplaceAll(strings.ToLower(filepath.Base(path)), "-", "_")
	return n == "language_server" || strings.HasPrefix(n, "language_server_")
}

// Enumerate executable names first. Read argv only for current-user language
// servers and never include argv or tokens in diagnostics.
func languageEndpoints(ctx context.Context) ([]localEndpoint, error) {
	uid := strconv.Itoa(os.Getuid())
	b, err := commandOutput(ctx, 4*time.Second, "ps", "-ww", "-U", uid, "-o", "pid=,comm=")
	if err != nil {
		return nil, err
	}
	var serverPID int
	var flags map[string]string
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		exe := strings.Join(fields[1:], " ")
		var f map[string]string
		switch {
		case languageServerName(exe):
			args, err := commandOutput(ctx, 3*time.Second, "ps", "-ww", "-p", fields[0], "-o", "uid=,args=")
			if err != nil {
				return nil, fmt.Errorf("cannot establish language server identity: %w", err)
			}
			words := strings.Fields(string(args))
			if len(words) < 2 || words[0] != uid {
				continue
			}
			f = antigravityFlags(words[1:])
			if f == nil {
				continue
			}
		case filepath.Base(exe) == "agy" || filepath.Base(exe) == "antigravity-cli" || filepath.Base(exe) == "antigravity_cli":
			match, err := matchesInstalledAgy(ctx, pid, exe)
			if err != nil {
				return nil, fmt.Errorf("cannot verify running agy: %w", err)
			}
			if !match {
				continue
			}
			// A matched CLI uses anonymous RPC. Never read its arguments or
			// attach credentials, even if its listener asks for them.
		default:
			continue
		}
		if serverPID != 0 && serverPID != pid {
			return nil, errors.New("multiple Antigravity language servers: cannot identify the active account")
		}
		serverPID, flags = pid, f
	}
	if serverPID == 0 || (flags != nil && flags["csrf_token"] == "") {
		return nil, errors.New("no verified running agy or authenticated Antigravity language server")
	}
	owned, err := antigravityListeners(ctx, serverPID)
	if err != nil {
		return nil, err
	}
	eps := languageServerEndpoints(serverPID, flags, owned)
	if len(eps) == 0 {
		return nil, errors.New("no verified Antigravity language server loopback listener")
	}
	return eps, nil
}

func antigravityFlags(args []string) map[string]string {
	firstFlag := len(args)
	flags := map[string]string{}
	for i, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		firstFlag = min(firstFlag, i)
		k, v, found := strings.Cut(arg, "=")
		if !found && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			v = args[i+1]
		}
		flags[strings.ReplaceAll(k[2:], "-", "_")] = v
	}
	exe := strings.Join(args[:firstFlag], " ")
	if !languageServerName(exe) {
		return nil
	}
	for _, component := range strings.Split(strings.ToLower(exe), "/") {
		switch component {
		case "antigravity", "antigravity-ide", "antigravity.app", "antigravity ide.app", "gemini.app":
			return flags
		}
	}
	switch strings.ToLower(filepath.Base(flags["app_data_dir"])) {
	case "antigravity", "antigravity-ide":
		return flags
	}
	return nil
}

func languageServerEndpoints(pid int, flags map[string]string, owned []netip.AddrPort) []localEndpoint {
	var advertised, dynamic []localEndpoint
	for _, addr := range owned {
		port := strconv.Itoa(int(addr.Port()))
		if port == flags["extension_server_port"] {
			continue // IDE callback uses a different service and credential.
		}
		schemes := []string{"https", "http"}
		explicit := false
		switch port {
		case flags["https_port"]:
			schemes, explicit = []string{"https"}, true
		case flags["http_port"]:
			schemes, explicit = []string{"http"}, true
		}
		for _, scheme := range schemes {
			ep := localEndpoint{pid: pid, addr: addr, scheme: scheme, token: flags["csrf_token"], advertised: explicit}
			if explicit {
				advertised = append(advertised, ep)
			} else {
				dynamic = append(dynamic, ep)
			}
		}
	}
	return append(advertised, dynamic...)
}

const rpcPrefix = "/exa.language_server_pb.LanguageServerService/"

func rpcCall(ctx context.Context, client *http.Client, ep localEndpoint, method, body string) ([]byte, error) {
	target := ep.scheme + "://" + ep.addr.String() + rpcPrefix + method
	if ep.token != "" && !ep.advertised {
		// A process can also own listeners for unrelated services. Try the
		// read-only Connect method without credentials. Only a CSRF challenge
		// at this method authorizes retrying with the language server token.
		raw, err := doRPC(ctx, client, target, method, body, "")
		if !errors.Is(err, errCSRFRequired) {
			return raw, err
		}
	}
	return doRPC(ctx, client, target, method, body, ep.token)
}

var errCSRFRequired = errors.New("language server requires CSRF token")

// Only retry a dynamic listener on an explicit CSRF authentication failure.
// Generic HTTP errors and login forms must never prompt credential delivery.
func csrfChallenge(raw []byte) bool {
	message := strings.ToLower(strings.TrimSpace(string(raw)))
	if strings.HasPrefix(message, "{") {
		var failure struct{ Code, Message string }
		if json.Unmarshal(raw, &failure) != nil || (failure.Code != "unauthenticated" && failure.Code != "permission_denied" && failure.Code != "invalid_argument") {
			return false
		}
		message = strings.ToLower(strings.TrimSpace(failure.Message))
	}
	switch message {
	case "invalid csrf token", "missing csrf token", "csrf token required",
		"invalid x-codeium-csrf-token", "missing x-codeium-csrf-token",
		"missing x-codeium-csrf-token header", "invalid x-codeium-csrf-token header":
		return true
	}
	return false
}

func doRPC(ctx context.Context, client *http.Client, target, method, body, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", target, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	if token != "" {
		req.Header.Set("X-Codeium-Csrf-Token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if token == "" && (resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403) {
			challenge, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
			if err != nil {
				return nil, err
			}
			if csrfChallenge(challenge) {
				return nil, errCSRFRequired
			}
		}
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
	if len(endpoints) == 0 {
		return nil, errors.New("no Antigravity language server")
	}
	pid := endpoints[0].pid
	for _, ep := range endpoints {
		if ep.pid != pid || !ep.addr.Addr().IsLoopback() {
			return nil, errors.New("ambiguous or non-loopback Antigravity server")
		}
	}
	// Self-signed local TLS. Check ownership again *after* dialing and before
	// sending HTTP (including tokens), in case a process exited or rebound.
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			var dialer net.Dialer
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			listeners, err := antigravityListeners(ctx, pid)
			peer := conn.RemoteAddr().(*net.TCPAddr).AddrPort()
			if err != nil || !slices.Contains(listeners, netip.AddrPortFrom(peer.Addr().Unmap(), peer.Port())) {
				conn.Close()
				return nil, errors.Join(errors.New("Antigravity listener ownership changed"), err)
			}
			return conn, nil
		},
	}
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
