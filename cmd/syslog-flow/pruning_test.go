package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPruningDefaultsAndBackgroundChecks(t *testing.T) {
	data, err := defaultSettingsJSON("pruning")
	if err != nil {
		t.Fatal(err)
	}
	config, err := parsePruningConfig(data)
	if err != nil || config.Enabled || config.MaxAgeDays != 0 || config.MaxSizeMiB != 0 {
		t.Fatalf("unsafe pruning defaults: %+v, %v", config, err)
	}
	pruner := testPruner(t, pruningConfig{Enabled: true, MaxAgeDays: 1})
	first := pruningFixture(t, pruner, "2020/01/01", 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pruner.start(ctx)
	waitRemoved := func(path string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("background pruning did not remove expired day")
	}
	waitRemoved(first)
	// Add another day after the startup scan, then request another check.
	pruner.mu.Lock()
	second := pruningFixture(t, pruner, "2020/01/02", 1)
	pruner.mu.Unlock()
	pruner.wake <- struct{}{}
	waitRemoved(second)
}

func testPruner(t *testing.T, config pruningConfig) *logPruner {
	t.Helper()
	pruner := &logPruner{root: t.TempDir(), path: filepath.Join(t.TempDir(), "pruning.json"), index: &logIndex{files: make(map[string]*indexedFile)}, wake: make(chan struct{}, 1)}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pruner.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return pruner
}

func pruningFixture(t *testing.T, pruner *logPruner, day string, size int64) string {
	t.Helper()
	path := filepath.Join(pruner.root, filepath.FromSlash(day), "router.log")
	appendActionLog(t, path, "")
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
	pruner.index.files[path] = &indexedFile{day: day, name: "router.log", path: path}
	return path
}

func TestPruningLimits(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, appLocation)
	days := []string{"2026/10/17", "2026/10/18", "2026/10/19", "2026/10/20", "2026/10/21"}
	for _, tc := range []struct {
		name   string
		config pruningConfig
		keep   int
	}{
		{"off", pruningConfig{}, 0},
		{"off with limits", pruningConfig{MaxAgeDays: 1, MaxSizeMiB: 1}, 0},
		{"age", pruningConfig{Enabled: true, MaxAgeDays: 3}, 1},
		{"size", pruningConfig{Enabled: true, MaxSizeMiB: 1}, 2},
		{"both", pruningConfig{Enabled: true, MaxAgeDays: 3, MaxSizeMiB: 1}, 2},
		{"age stricter", pruningConfig{Enabled: true, MaxAgeDays: 1, MaxSizeMiB: 1}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pruner := testPruner(t, tc.config)
			for _, day := range days {
				pruningFixture(t, pruner, day, 300*1024)
			}
			if err := pruner.run(now); err != nil {
				t.Fatal(err)
			}
			for i, day := range days {
				path := filepath.Join(pruner.root, filepath.FromSlash(day))
				_, err := os.Stat(path)
				if i < tc.keep {
					if !os.IsNotExist(err) {
						t.Errorf("old day %s still exists: %v", day, err)
					}
				} else if err != nil {
					t.Errorf("retained day %s: %v", day, err)
				}
			}
			if len(pruner.index.files) != len(days)-tc.keep {
				t.Fatal("removed days remain in the viewer index")
			}
			if err := pruner.run(now); err != nil {
				t.Fatalf("second check failed: %v", err)
			}
		})
	}
}

func TestPruningProtectsTodayAndSizeBoundary(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, appLocation)
	pruner := testPruner(t, pruningConfig{Enabled: true, MaxSizeMiB: 1})
	old := pruningFixture(t, pruner, "2026/10/19", 512*1024)
	today := pruningFixture(t, pruner, "2026/10/20", 512*1024)
	if err := pruner.run(now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("size equal to limit should not trigger deletion")
	}
	if err := os.Truncate(today, 2*1024*1024); err != nil {
		t.Fatal(err)
	}
	if err := pruner.run(now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old day should be removed when over limit")
	}
	if _, err := os.Stat(today); err != nil {
		t.Fatal("today must be preserved even when it alone exceeds the limit")
	}
}

func TestPruningAgeUsesCalendarDaysAcrossDST(t *testing.T) {
	location, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 9, 0, 1, 0, 0, location)
	pruner := testPruner(t, pruningConfig{Enabled: true, MaxAgeDays: 2})
	old := pruningFixture(t, pruner, "2026/03/07", 1)
	kept := pruningFixture(t, pruner, "2026/03/08", 1)
	if err := pruner.run(now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("day outside retention remains")
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatal("yesterday should survive the DST transition")
	}
}

func TestPruningRejectsInvalidConfigWithoutDeleting(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{}`, `{"enabled":true}`, `{"enabled":true,"max_age_days":-1}`, `{"max_size_mib":8796093022208}`, `{"enabled":true,"max_age_days":1,"typo":true}`, `{} {}`} {
		t.Run(raw, func(t *testing.T) {
			pruner := testPruner(t, pruningConfig{})
			path := pruningFixture(t, pruner, "2020/01/01", 1)
			if err := os.WriteFile(pruner.path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			err := pruner.run(appNow())
			if raw == `{}` {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || pruner.lastError == "" {
				t.Fatal("invalid config should report an error")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("invalid or disabled settings must not delete logs")
			}
		})
	}
}

func TestPruningDoesNotFollowSymlinksOrDeleteInvalidDates(t *testing.T) {
	pruner := testPruner(t, pruningConfig{Enabled: true, MaxAgeDays: 1})
	outside := t.TempDir()
	appendActionLog(t, filepath.Join(outside, "01/01/router.log"), "keep")
	if err := os.Symlink(outside, filepath.Join(pruner.root, "2020")); err != nil {
		t.Fatal(err)
	}
	invalid := pruningFixture(t, pruner, "2026/02/30", 1)
	old := pruningFixture(t, pruner, "2026/01/01", 1)
	if err := os.Symlink(outside, filepath.Join(filepath.Dir(old), "linked")); err != nil {
		t.Fatal(err)
	}
	if err := pruner.run(appNow()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{invalid, filepath.Join(outside, "01/01/router.log")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unrelated file removed: %s: %v", path, err)
		}
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("valid expired day was not deleted")
	}
}

func TestPruningSettings(t *testing.T) {
	previous := pruning
	pruning = testPruner(t, pruningConfig{})
	t.Cleanup(func() { pruning = previous })
	page := httptest.NewRecorder()
	handleSettings(page, httptest.NewRequest(http.MethodGet, "/settings?section=pruning", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Save Pruning") || strings.Contains(page.Body.String(), `name="enabled" checked`) {
		t.Fatal("pruning settings should render off by default")
	}
	for _, tc := range []struct {
		name, csrf, age, size string
		enabled               bool
		status                int
	}{
		{"bad csrf", "bad", "30", "", true, 400},
		{"missing limits", settingsCSRFToken, "", "", true, 400},
		{"invalid age", settingsCSRFToken, "1.5", "", true, 400},
		{"negative size", settingsCSRFToken, "", "-1", true, 400},
		{"valid both", settingsCSRFToken, "30", "1024", true, 200},
		{"disable", settingsCSRFToken, "", "", false, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"section": {"pruning"}, "csrf": {tc.csrf}, "max_age_days": {tc.age}, "max_size_mib": {tc.size}}
			if tc.enabled {
				form.Set("enabled", "on")
			}
			request := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Accept", "application/json")
			response := httptest.NewRecorder()
			handleSettings(response, request)
			if response.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
			if tc.status == 200 {
				config, err := pruning.load()
				if err != nil || config.Enabled != tc.enabled {
					t.Fatalf("settings not persisted: %+v, %v", config, err)
				}
				select {
				case <-pruning.wake:
				default:
					t.Fatal("saving should request a pruning check")
				}
			}
		})
	}
}
