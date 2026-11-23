package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Compare the executable mapped by an already-running CLI with the installed
// agy found on PATH. This lookup never starts agy or sends it a prompt.
func matchesInstalledAgy(ctx context.Context, pid int, comm string) (bool, error) {
	path, err := exec.LookPath("agy")
	if errors.Is(err, exec.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	known, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	switch runtime.GOOS {
	case "linux":
		image, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
		if err != nil {
			return false, err
		}
		return os.SameFile(image, known), nil
	case "darwin":
		// Some ps versions give only a basename. Regardless of its form,
		// require the installed device+inode in the PID's executable texts.
		if filepath.IsAbs(comm) {
			image, err := os.Stat(comm)
			if err != nil || !os.SameFile(image, known) {
				return false, err
			}
		}
		report, err := commandOutput(ctx, 3*time.Second, "/usr/sbin/lsof", "-nP", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-FpfDi")
		if err != nil {
			return false, err
		}
		st := known.Sys().(*syscall.Stat_t)
		var matchPID, matchDev bool
		for _, field := range strings.Split(string(report), "\n") {
			switch {
			case strings.HasPrefix(field, "p"):
				matchPID = field[1:] == strconv.Itoa(pid)
			case strings.HasPrefix(field, "f"):
				matchDev = false
			case strings.HasPrefix(field, "D"):
				dev, err := strconv.ParseUint(field[1:], 0, 64)
				matchDev = err == nil && dev == uint64(uint32(st.Dev))
			case matchPID && matchDev && field == "i"+strconv.FormatUint(uint64(st.Ino), 10):
				return true, nil
			}
		}
	}
	return false, nil
}

// Return only loopback addresses bound by this language server PID. Never
// infer ownership from a port number alone (including one found in argv).
func antigravityListeners(ctx context.Context, pid int) ([]netip.AddrPort, error) {
	switch runtime.GOOS {
	case "linux":
		return linuxLanguageServerListeners(ctx, pid)
	case "darwin":
		b, err := commandOutput(ctx, 3*time.Second, "/usr/sbin/lsof", "-nP", "-a", "-p", strconv.Itoa(pid), "-iTCP", "-sTCP:LISTEN", "-Fpn")
		if err != nil {
			return nil, fmt.Errorf("language server listeners: %w", err)
		}
		return lsofLanguageServerListeners(string(b), pid), nil
	default:
		return nil, errors.New("Antigravity listener ownership lookup unsupported on this OS")
	}
}

func lsofLanguageServerListeners(report string, pid int) []netip.AddrPort {
	var listeners []netip.AddrPort
	match := false
	for _, field := range strings.Split(report, "\n") {
		switch {
		case strings.HasPrefix(field, "p"):
			match = field[1:] == strconv.Itoa(pid)
		case match && strings.HasPrefix(field, "n"):
			addr, err := netip.ParseAddrPort(field[1:])
			if err == nil {
				listeners = addLoopbackListener(listeners, addr.Addr(), addr.Port())
			}
		}
	}
	return listeners
}

func addLoopbackListener(list []netip.AddrPort, ip netip.Addr, port uint16) []netip.AddrPort {
	addr := netip.AddrPortFrom(ip.Unmap(), port)
	if addr.Addr().IsLoopback() && port != 0 && !slices.Contains(list, addr) {
		return append(list, addr)
	}
	return list
}

func linuxLanguageServerListeners(ctx context.Context, pid int) ([]netip.AddrPort, error) {
	process := filepath.Join("/proc", strconv.Itoa(pid))
	targetNS, err := os.Stat(filepath.Join(process, "ns/net"))
	if err != nil {
		return nil, err
	}
	ourNS, err := os.Stat("/proc/self/ns/net")
	if err != nil {
		return nil, err
	}
	if !os.SameFile(targetNS, ourNS) {
		return nil, errors.New("Antigravity is in a different network namespace")
	}
	dir := filepath.Join(process, "fd")
	fds, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	inodes := make(map[string]bool)
	for _, fd := range fds {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		target, err := os.Readlink(filepath.Join(dir, fd.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue // FD closed while enumerating.
		}
		if err != nil {
			return nil, err
		}
		if inode, ok := strings.CutPrefix(target, "socket:["); ok && strings.HasSuffix(inode, "]") {
			inodes[strings.TrimSuffix(inode, "]")] = true
		}
	}
	var listeners []netip.AddrPort
	for _, family := range []string{"tcp", "tcp6"} {
		f, err := os.Open(filepath.Join(process, "net", family))
		if family == "tcp6" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		rows := bufio.NewScanner(f)
		for rows.Scan() {
			columns := strings.Fields(rows.Text())
			if len(columns) < 10 || columns[3] != "0A" || !inodes[columns[9]] {
				continue
			}
			if addr, ok := procListenAddr(columns[1]); ok {
				listeners = addLoopbackListener(listeners, addr.Addr(), addr.Port())
			}
		}
		err = errors.Join(rows.Err(), f.Close(), ctx.Err())
		if err != nil {
			return nil, err
		}
	}
	return listeners, nil
}

// procfs prints addresses as one (IPv4) or four (IPv6) native-endian uint32s.
func procListenAddr(endpoint string) (netip.AddrPort, bool) {
	raw, portText, ok := strings.Cut(endpoint, ":")
	if !ok || (len(raw) != 8 && len(raw) != 32) {
		return netip.AddrPort{}, false
	}
	port, err := strconv.ParseUint(portText, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	var addr [16]byte
	for i := 0; i < len(raw); i += 8 {
		word, err := strconv.ParseUint(raw[i:i+8], 16, 32)
		if err != nil {
			return netip.AddrPort{}, false
		}
		binary.NativeEndian.PutUint32(addr[i/2:], uint32(word))
	}
	ip, ok := netip.AddrFromSlice(addr[:len(raw)/2])
	return netip.AddrPortFrom(ip.Unmap(), uint16(port)), ok
}
