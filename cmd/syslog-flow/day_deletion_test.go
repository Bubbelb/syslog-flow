package main

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeleteDayRemovesWholeFolderAndUpdatesIndex(t *testing.T) {
	pruner := testPruner(t, pruningConfig{})
	day := appNow().Format("2006/01/02")
	path := pruningFixture(t, pruner, day, 100)
	other := pruningFixture(t, pruner, "2020/01/01", 100)
	appendActionLog(t, filepath.Join(filepath.Dir(path), "summary.json"), "{}")
	pruner.index.generation = "before-delete"
	reopened := false
	pruner.reopen = func() error {
		reopened = true
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatal("rsyslog should reopen only after the day has been removed")
		}
		return nil
	}
	if err := pruner.deleteDay(day); err != nil {
		t.Fatal(err)
	}
	if !reopened || pruner.index.generation == "before-delete" {
		t.Fatal("deletion must reopen log files and invalidate open viewers")
	}
	if len(pruner.index.files) != 1 || pruner.index.files[other] == nil {
		t.Fatal("deletion must remove only the selected day's index entries")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("another day's logs were deleted")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("the entire day directory must be deleted, including summaries")
	}
}

func TestDeleteDayRejectsInvalidPathsAndSymlinks(t *testing.T) {
	pruner := testPruner(t, pruningConfig{})
	outside := t.TempDir()
	appendActionLog(t, filepath.Join(outside, "keep.log"), "keep")
	if err := os.MkdirAll(filepath.Join(pruner.root, "2020/01"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(pruner.root, "2020/01/01")); err != nil {
		t.Fatal(err)
	}
	for _, day := range []string{".", "../outside", "2020/01/01/../02", "2026/02/30", "2020/01/01"} {
		if err := pruner.deleteDay(day); err == nil {
			t.Fatalf("accepted invalid or linked day %q", day)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.log")); err != nil {
		t.Fatal("deletion escaped the log directory")
	}
}

func TestDeleteDayReportsReopenFailure(t *testing.T) {
	pruner := testPruner(t, pruningConfig{})
	pruningFixture(t, pruner, "2020/01/01", 1)
	pruner.reopen = func() error { return errors.New("test reopen failure") }
	err := pruner.deleteDay("2020/01/01")
	if err == nil || !strings.Contains(err.Error(), "day deleted, but unable to reopen") || len(pruner.index.files) != 0 {
		t.Fatalf("reopen failure must explain that deletion already happened: %v", err)
	}
}

func TestDayDeleteEndpoint(t *testing.T) {
	previous := pruning
	pruning = testPruner(t, pruningConfig{})
	t.Cleanup(func() { pruning = previous })
	path := pruningFixture(t, pruning, "2020/01/01", 1)
	for _, tc := range []struct {
		method, action, csrf string
		status               int
	}{
		{"POST", "delete", "bad", 400},
		{"POST", "", settingsCSRFToken, 400},
		{"DELETE", "delete", settingsCSRFToken, 405},
		{"POST", "delete", settingsCSRFToken, 303},
	} {
		form := url.Values{"action": {tc.action}, "csrf": {tc.csrf}}
		request := httptest.NewRequest(tc.method, "/day/2020/01/01", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handleDay(response, request)
		if response.Code != tc.status {
			t.Fatalf("%s %s: status %d, want %d: %s", tc.method, tc.action, response.Code, tc.status, response.Body.String())
		}
		if tc.status == 303 {
			if response.Header().Get("Location") != "/" {
				t.Fatal("successful deletion should return to the homepage")
			}
		} else if _, err := os.Stat(path); err != nil {
			t.Fatal("invalid request deleted logs")
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("confirmed POST did not delete the log day")
	}
}

func TestDayDeleteButton(t *testing.T) {
	var output bytes.Buffer
	if err := page.Execute(&output, PageData{Selected: "2020/01/01", CSRF: "test-token"}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`data-delete-day="2020/01/01"`, `action="/day/2020/01/01" method="post"`, `name="csrf" value="test-token"`, `class="delete-day-button"`, `window.confirm(`} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing delete button protection or layout: %s", expected)
		}
	}
}

func TestReopenSyslogRejectsUnsafePID(t *testing.T) {
	for _, value := range []string{"0", "1", "-10", "garbage"} {
		t.Setenv("SYSLOG_FLOW_RSYSLOG_PID", value)
		if err := reopenSyslogFiles(); err == nil {
			t.Fatalf("unsafe PID %q accepted", value)
		}
	}
	t.Setenv("SYSLOG_FLOW_RSYSLOG_PID", "")
	if err := reopenSyslogFiles(); err != nil {
		t.Fatal("standalone Go viewer should not require rsyslog")
	}
}
