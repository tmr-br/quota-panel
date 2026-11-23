package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type cappedOutput struct {
	buf bytes.Buffer
	max int
}

func (w *cappedOutput) Write(p []byte) (int, error) {
	if len(p) > w.max-w.buf.Len() {
		return 0, errors.New("command response too large")
	}
	return w.buf.Write(p)
}

func commandOutput(ctx context.Context, timeout time.Duration, exe string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	// npx may spawn node and agy may spawn a helper. Cancel the whole
	// invocation so a stuck provider cannot accumulate background processes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	var buf cappedOutput
	buf.max = 4 << 20
	cmd.Stdout = &buf
	cmd.Stderr = io.Discard
	var versionErr cappedOutput
	if len(args) == 1 && args[0] == "--version" {
		versionErr.max = 16 << 10
		cmd.Stderr = &versionErr
	}
	if len(args) > 0 && (args[0] == "--version" || args[0] == "-p") {
		cmd.Env = append(os.Environ(), "AGY_CLI_DISABLE_AUTO_UPDATE=true")
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(exe), ctx.Err())
		}
		return nil, fmt.Errorf("%s: %w", filepath.Base(exe), err)
	}
	if versionErr.buf.Len() > 0 {
		buf.buf.WriteByte('\n')
		buf.buf.Write(versionErr.buf.Bytes())
	}
	return buf.buf.Bytes(), nil
}
