package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

func handlePruningSave(w http.ResponseWriter, r *http.Request) {
	config := pruningConfig{Enabled: r.Form.Get("enabled") == "on"}
	if value := strings.TrimSpace(r.Form.Get("max_size_mib")); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			writeSettingsError(w, r, http.StatusBadRequest, "Maximum size must be a whole number of MiB")
			return
		}
		config.MaxSizeMiB = parsed
	}
	if value := strings.TrimSpace(r.Form.Get("max_age_days")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			writeSettingsError(w, r, http.StatusBadRequest, "Maximum age must be a whole number of days")
			return
		}
		config.MaxAgeDays = parsed
	}
	data, _ := json.MarshalIndent(config, "", "  ")
	if _, err := parsePruningConfig(data); err != nil {
		writeSettingsError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	pruning.mu.Lock()
	err := writeSettingsFile(pruning.path, append(data, '\n'))
	pruning.mu.Unlock()
	if err != nil {
		writeSettingsError(w, r, http.StatusInternalServerError, "Unable to save pruning settings")
		return
	}
	select {
	case pruning.wake <- struct{}{}:
	default:
	}
	if wantsSettingsJSON(r) {
		writeSettingsJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	http.Redirect(w, r, "/settings?section=pruning", http.StatusSeeOther)
}
