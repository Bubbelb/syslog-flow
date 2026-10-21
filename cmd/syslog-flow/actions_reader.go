package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxActionLineSize = 1024 * 1024

type actionFileCursor struct {
	info        os.FileInfo
	offset      int64
	skipPartial bool
}

type actionReader struct {
	root    string
	cursors map[string]*actionFileCursor
	engine  *actionEngine
}

func (engine *actionEngine) start(ctx context.Context, root string) error {
	reader := &actionReader{root: root, cursors: make(map[string]*actionFileCursor), engine: engine}
	// Establish positions before accepting requests; existing logs never trigger actions.
	if err := reader.poll(appNow(), true); err != nil {
		return err
	}
	// Finish each delivery before starting the next queued action.
	go engine.worker(ctx)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				_ = engine.reload()
				engine.setReaderError(reader.poll(now.In(appLocation), false))
			}
		}
	}()
	return nil
}

func (reader *actionReader) poll(now time.Time, initial bool) error {
	active := reader.engine.active()
	days := []string{now.AddDate(0, 0, -1).Format("2006/01/02"), now.Format("2006/01/02")}
	var firstError error
	// Yesterday remains monitored across midnight, including late rsyslog writes.
	for _, day := range days {
		dir := filepath.Join(reader.root, day)
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			if firstError == nil {
				firstError = err
			}
			continue
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".log") || !validName(entry.Name()) {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if err := reader.readFile(path, initial || !active, now); err != nil && firstError == nil {
				firstError = err
			}
		}
	}
	for path := range reader.cursors {
		// Keep positions through transient discovery failures to avoid replaying a file.
		if filepath.Dir(path) != filepath.Join(reader.root, days[0]) && filepath.Dir(path) != filepath.Join(reader.root, days[1]) {
			delete(reader.cursors, path)
		}
	}
	return firstError
}

func (reader *actionReader) readFile(path string, skipExisting bool, now time.Time) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	cursor := reader.cursors[path]
	if cursor != nil && os.SameFile(cursor.info, info) && cursor.offset == info.Size() {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	// Stat the opened file, so replacement between discovery and open is harmless.
	info, err = file.Stat()
	if err != nil {
		return err
	}
	if cursor == nil || !os.SameFile(cursor.info, info) || info.Size() < cursor.offset {
		cursor = &actionFileCursor{}
		reader.cursors[path] = cursor
	}
	cursor.info = info
	if skipExisting {
		cursor.offset = info.Size()
		cursor.skipPartial = false
		if cursor.offset > 0 {
			var last [1]byte
			if _, err := file.ReadAt(last[:], cursor.offset-1); err != nil {
				return err
			}
			cursor.skipPartial = last[0] != '\n'
		}
		return nil
	}
	if _, err := file.Seek(cursor.offset, io.SeekStart); err != nil {
		return err
	}
	scanner := bufio.NewScanner(io.LimitReader(file, info.Size()-cursor.offset))
	scanner.Buffer(make([]byte, 0, 64*1024), maxActionLineSize+1)
	skippedOversized := false
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if end := bytes.IndexByte(data, '\n'); end >= 0 {
			cursor.offset += int64(end + 1)
			if cursor.skipPartial {
				cursor.skipPartial = false
				return end + 1, nil, nil
			}
			return end + 1, bytes.TrimSuffix(data[:end], []byte{'\r'}), nil
		}
		if len(data) > maxActionLineSize {
			skippedOversized = true
			cursor.skipPartial = true
		}
		if cursor.skipPartial && len(data) > 0 {
			// Discard through the next newline, even when it arrives in a later poll.
			cursor.offset += int64(len(data))
			return len(data), nil, nil
		}
		return 0, nil, nil
	})
	for scanner.Scan() {
		reader.engine.consider(scanner.Text(), now)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if skippedOversized {
		return fmt.Errorf("%s: skipped log line exceeding 1 MiB", filepath.Base(path))
	}
	return nil
}
