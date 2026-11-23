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
	"time"
)

// Cache decoded usage records or a parse failure for this exact file version.
// Changed/replaced files are decoded again. I/O errors are always retried.
type logFile[T any] struct {
	info      os.FileInfo
	records   []T
	malformed error
}
type logCache[T any] map[string]logFile[T]
type logParser[T any] func(path string, line []byte) (T, bool, error)

func (cache logCache[T]) read(ctx context.Context, root string, parse logParser[T]) ([]T, error) {
	return cache.readReportingMalformed(ctx, root, parse, nil)
}

func (cache logCache[T]) readReportingMalformed(ctx context.Context, root string, parse logParser[T], report func(time.Time, error)) ([]T, error) {
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
		if file.malformed != nil {
			if report == nil {
				return file.malformed
			}
			report(file.info.ModTime(), file.malformed)
			return nil
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

func readLog[T any](ctx context.Context, path string, parse logParser[T]) (logFile[T], error) {
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
	// Decode only the bytes belonging to the recorded snapshot.
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
		v, relevant, err := parse(path, raw)
		if err != nil {
			result.records = nil
			result.malformed = fmt.Errorf("%s:%d: %w", path, line, err)
			return result, nil
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
