package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Cache just the decoded usage records. Prompt text is never retained. A
// changed/replaced file is decoded again; a failed read never enters the cache.
type logFile[T any] struct {
	info    os.FileInfo
	records []T
}
type logCache[T any] map[string]logFile[T]

func (cache logCache[T]) read(ctx context.Context, root string, parse func([]byte) (T, bool, error)) ([]T, error) {
	dir, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	var all []T
	seen := map[string]bool{}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		seen[path] = true
		file, ok := cache[path]
		if !ok || !os.SameFile(info, file.info) || info.Size() != file.info.Size() || !info.ModTime().Equal(file.info.ModTime()) {
			file, err = readLog(ctx, path, parse)
			if err != nil {
				return err
			}
			cache[path] = file
		}
		all = append(all, file.records...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for path := range cache {
		if !seen[path] {
			delete(cache, path)
		}
	}
	return all, nil
}

func readLog[T any](ctx context.Context, path string, parse func([]byte) (T, bool, error)) (logFile[T], error) {
	var result logFile[T]
	f, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer f.Close()
	result.info, err = f.Stat()
	if err != nil {
		return result, err
	}
	// Ignore bytes appended after this snapshot. A torn JSON record makes the
	// source unavailable for this poll and is retried once its writer finishes.
	scan := bufio.NewScanner(io.LimitReader(f, result.info.Size()))
	scan.Buffer(make([]byte, 64<<10), 64<<20)
	for line := 1; scan.Scan(); line++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		raw := bytes.TrimSpace(scan.Bytes())
		if len(raw) == 0 {
			continue
		}
		v, relevant, err := parse(raw)
		if err != nil {
			return result, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if relevant {
			result.records = append(result.records, v)
		}
	}
	if err := scan.Err(); err != nil {
		return result, fmt.Errorf("%s: %w", path, err)
	}
	return result, nil
}
