package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const pruningConfigPath = "/config/pruning.json"

type pruningConfig struct {
	Enabled    bool  `json:"enabled"`
	MaxAgeDays int   `json:"max_age_days"`
	MaxSizeMiB int64 `json:"max_size_mib"`
}

type logPruner struct {
	mu        sync.Mutex
	root      string
	path      string
	index     *logIndex
	wake      chan struct{}
	lastError string
}

var pruning = &logPruner{root: logRoot, path: pruningConfigPath, index: stateIndex, wake: make(chan struct{}, 1)}

func parsePruningConfig(data []byte) (pruningConfig, error) {
	var config pruningConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, fmt.Errorf("invalid pruning settings: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return config, fmt.Errorf("pruning settings must contain one JSON object")
	}
	if config.MaxAgeDays < 0 || config.MaxAgeDays > 36500 {
		return config, fmt.Errorf("maximum age must be between 0 and 36500 days")
	}
	if config.MaxSizeMiB < 0 || config.MaxSizeMiB > (1<<63-1)/(1<<20) {
		return config, fmt.Errorf("maximum size is outside the supported range")
	}
	if config.Enabled && config.MaxAgeDays == 0 && config.MaxSizeMiB == 0 {
		return config, fmt.Errorf("set an age or size limit before enabling pruning")
	}
	return config, nil
}

func (pruner *logPruner) load() (pruningConfig, error) {
	data, err := os.ReadFile(pruner.path)
	if os.IsNotExist(err) {
		return pruningConfig{}, nil
	}
	if err != nil {
		return pruningConfig{}, err
	}
	return parsePruningConfig(data)
}

func (pruner *logPruner) start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			_ = pruner.run(appNow())
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-pruner.wake:
			}
		}
	}()
}

type pruningDay struct {
	name string
	size int64
}

// WalkDir does not follow symlinks; Root confines deletion to the logs tree.
func pruningDays(root *os.Root) ([]pruningDay, int64, error) {
	days := make(map[string]int64)
	var total int64
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		parts := strings.Split(path, "/")
		if entry.IsDir() && len(parts) <= 3 {
			if len(parts) == 3 {
				if _, err := time.Parse("2006/01/02", path); err != nil {
					return fs.SkipDir
				}
				days[path] = 0
			} else {
				width := 2
				if len(parts) == 1 {
					width = 4
				}
				if len(entry.Name()) != width || strings.Trim(entry.Name(), "0123456789") != "" {
					return fs.SkipDir
				}
			}
		}
		if len(parts) > 3 && entry.Type().IsRegular() {
			day := strings.Join(parts[:3], "/")
			if _, ok := days[day]; ok {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				days[day] += info.Size()
				total += info.Size()
			}
		}
		return nil
	})
	result := make([]pruningDay, 0, len(days))
	for day, size := range days {
		result = append(result, pruningDay{name: day, size: size})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].name < result[j].name })
	return result, total, err
}

func (pruner *logPruner) run(now time.Time) (err error) {
	pruner.mu.Lock()
	defer pruner.mu.Unlock()
	defer func() {
		message := ""
		if err != nil {
			message = err.Error()
			if message != pruner.lastError {
				log.Printf("Automatic pruning: %s", message)
			}
		}
		pruner.lastError = message
	}()
	config, err := pruner.load()
	if err != nil || !config.Enabled {
		return err
	}
	// Serialize with index refreshes so removed days cannot be cached again.
	pruner.index.refreshMu.Lock()
	defer pruner.index.refreshMu.Unlock()
	root, err := os.OpenRoot(pruner.root)
	if err != nil {
		return err
	}
	defer root.Close()
	days, total, err := pruningDays(root)
	if err != nil {
		return err
	}
	today := now.Format("2006/01/02")
	oldestKept := now.AddDate(0, 0, 1-config.MaxAgeDays).Format("2006/01/02")
	for _, day := range days {
		if day.name >= today {
			break
		}
		tooOld := config.MaxAgeDays > 0 && day.name < oldestKept
		tooLarge := config.MaxSizeMiB > 0 && total > config.MaxSizeMiB*(1<<20)
		if !tooOld && !tooLarge {
			break
		}
		if err := root.RemoveAll(filepath.FromSlash(day.name)); err != nil {
			return fmt.Errorf("remove %s: %w", day.name, err)
		}
		total -= day.size
		pruner.index.mu.Lock()
		for path, file := range pruner.index.files {
			if file.day == day.name {
				delete(pruner.index.files, path)
			}
		}
		pruner.index.lastFullRefresh = time.Time{}
		pruner.index.mu.Unlock()
		log.Printf("Automatic pruning: deleted %s (%s)", day.name, formatBytes(day.size))
	}
	return nil
}
