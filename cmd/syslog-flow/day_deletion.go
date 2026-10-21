package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// The caller holds the pruner's mu and the index's refreshMu.
func (pruner *logPruner) deleteDayLocked(root *os.Root, day string) error {
	if !validDayPath(day) {
		return fmt.Errorf("invalid log day")
	}
	if _, err := time.Parse("2006/01/02", day); err != nil {
		return fmt.Errorf("invalid log day")
	}
	path := filepath.FromSlash(day)
	info, err := root.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && !info.IsDir() {
		return fmt.Errorf("log day is not a directory")
	}
	removeErr := root.RemoveAll(path)
	// Even a partially removed day must be rediscovered rather than cached.
	pruner.index.mu.Lock()
	for path, file := range pruner.index.files {
		if file.day == day {
			delete(pruner.index.files, path)
		}
	}
	pruner.index.lastRefresh = time.Time{}
	pruner.index.lastFullRefresh = time.Time{}
	pruner.index.changeGeneration()
	pruner.index.mu.Unlock()
	var reopenErr error
	if pruner.reopen != nil {
		reopenErr = pruner.reopen()
	}
	if removeErr != nil {
		return fmt.Errorf("remove %s: %w", day, removeErr)
	}
	if reopenErr != nil {
		return fmt.Errorf("day deleted, but unable to reopen syslog files: %w", reopenErr)
	}
	return nil
}

func (pruner *logPruner) deleteDay(day string) error {
	pruner.mu.Lock()
	defer pruner.mu.Unlock()
	pruner.index.refreshMu.Lock()
	defer pruner.index.refreshMu.Unlock()
	root, err := os.OpenRoot(pruner.root)
	if err != nil {
		return err
	}
	defer root.Close()
	return pruner.deleteDayLocked(root, day)
}

func reopenSyslogFiles() error {
	value := os.Getenv("SYSLOG_FLOW_RSYSLOG_PID")
	if value == "" {
		return nil // Running the Go viewer on its own has no managed rsyslog process.
	}
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 1 {
		return fmt.Errorf("invalid rsyslog PID")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer process.Release()
	return process.Signal(syscall.SIGHUP)
}

func handleDayDelete(w http.ResponseWriter, r *http.Request, day string) {
	if !validDayPath(day) {
		http.Error(w, "invalid log day", http.StatusBadRequest)
		return
	}
	if _, err := time.Parse("2006/01/02", day); err != nil {
		http.Error(w, "invalid log day", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil || r.PostForm.Get("action") != "delete" || r.PostForm.Get("csrf") != settingsCSRFToken {
		http.Error(w, "invalid day deletion request", http.StatusBadRequest)
		return
	}
	if err := pruning.deleteDay(day); err != nil {
		log.Printf("Delete day %s: %v", day, err)
		http.Error(w, "Unable to delete day: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("Deleted log day %s", day)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
