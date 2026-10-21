package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeviceColorsFormValidationAndOrder(t *testing.T) {
	valid := url.Values{
		"exact_match": {"router"}, "exact_color": {"#aabbcc"},
		"contains_match": {"WiFi", "Wi"}, "contains_color": {"#112233", "#445566"},
	}
	value, err := deviceColorsFormValue(valid)
	if err != nil {
		t.Fatal(err)
	}
	colors, err := parseDeviceColors([]byte(value))
	if err != nil || colors.Exact["router"] != "#AABBCC" || len(colors.Contains) != 2 || colors.Contains[0].Match != "WiFi" || colors.Contains[1].Match != "Wi" {
		t.Fatalf("colors or priority lost: %+v, %v", colors, err)
	}
	for _, form := range []url.Values{
		{"exact_match": {"router"}},
		{"exact_color": {"#112233"}},
		{"exact_match": {"../router"}, "exact_color": {"#112233"}},
		{"exact_match": {"router", "router"}, "exact_color": {"#112233", "#445566"}},
		{"contains_match": {"  "}, "contains_color": {"#112233"}},
		{"contains_match": {"router"}, "contains_color": {"red"}},
	} {
		if _, err := deviceColorsFormValue(form); err == nil {
			t.Fatalf("accepted invalid rules: %+v", form)
		}
	}
	value, err = deviceColorsFormValue(url.Values{})
	if err != nil || value != `{"exact":{},"contains":[]}` {
		t.Fatalf("removing all rules should save empty lists: %s, %v", value, err)
	}
}

func TestDeviceColorsEditorLoadsExistingFormats(t *testing.T) {
	for _, raw := range []string{
		`{"router":"#aabbcc"}`,
		`{"exact":{"router":"#aabbcc"},"contains":[{"match":"WiFi","color":"#112233"},{"match":"Wi","color":"#445566"}]}`,
	} {
		data := settingsData{Value: raw}
		populateDeviceColorsSettings(&data)
		if data.Error != "" || len(data.DeviceColorGroups) != 2 || len(data.DeviceColorGroups[0].Rows) != 1 || data.DeviceColorGroups[0].Rows[0].Color != "#AABBCC" {
			t.Fatalf("existing rules did not load: %+v", data)
		}
		if strings.Contains(raw, "contains") {
			rows := data.DeviceColorGroups[1].Rows
			if len(rows) != 2 || rows[0].Match != "WiFi" || rows[1].Match != "Wi" {
				t.Fatal("contains priority changed when loading")
			}
		}
	}
}

func TestDeviceColorsSettingsLifecycle(t *testing.T) {
	previous := settingsSections
	settingsSections = append(settingsSectionList(nil), previous...)
	t.Cleanup(func() { settingsSections = previous })
	path := filepath.Join(t.TempDir(), "device-colors.json")
	for i := range settingsSections {
		if settingsSections[i].ID == "device-colors" {
			settingsSections[i].Path = path
		}
	}
	if err := os.WriteFile(path, []byte(`{"router":"#aabbcc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRecorder()
	handleSettings(page, httptest.NewRequest(http.MethodGet, "/settings?section=device-colors", nil))
	body := page.Body.String()
	if strings.Contains(body, "<textarea") || !strings.Contains(body, `type="color"`) || !strings.Contains(body, `value="router"`) || !strings.Contains(body, "Save Device Colors") {
		t.Fatal("device colors should render a populated form instead of JSON")
	}
	post := func(form url.Values, status int) {
		t.Helper()
		form.Set("section", "device-colors")
		request := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Accept", "application/json")
		response := httptest.NewRecorder()
		handleSettings(response, request)
		if response.Code != status {
			t.Fatalf("status %d, want %d: %s", response.Code, status, response.Body.String())
		}
	}
	post(url.Values{"csrf": {"bad"}}, 400)
	post(url.Values{"csrf": {settingsCSRFToken}, "exact_match": {"router"}, "exact_color": {"red"}}, 400)
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != `{"router":"#aabbcc"}` {
		t.Fatal("invalid save must leave existing configuration untouched")
	}
	// An editor opened before the update may still submit its original JSON field.
	post(url.Values{"csrf": {settingsCSRFToken}, "value": {`{"router":"#aabbcc"}`}}, 200)
	contents, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	colors, err := parseDeviceColors(contents)
	if err != nil || colors.Exact["router"] != "#AABBCC" {
		t.Fatal("a save from the previous editor must preserve its rules")
	}
	post(url.Values{"csrf": {settingsCSRFToken}, "exact_match": {"router"}, "exact_color": {"#112233"}, "contains_match": {`WiFi"><script>alert(1)</script>`, "Wi"}, "contains_color": {"#445566", "#778899"}}, 200)
	contents, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	colors, err = parseDeviceColors(contents)
	if err != nil || colors.Exact["router"] != "#112233" || len(colors.Contains) != 2 || colors.Contains[1].Match != "Wi" {
		t.Fatalf("saved rules incorrect: %+v, %v", colors, err)
	}
	page = httptest.NewRecorder()
	handleSettings(page, httptest.NewRequest(http.MethodGet, "/settings?section=device-colors", nil))
	if strings.Contains(page.Body.String(), "<script>alert(1)</script>") {
		t.Fatal("contains rule rendered unescaped markup")
	}
	post(url.Values{"csrf": {settingsCSRFToken}, "action": {"reset"}}, 200)
	contents, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	colors, err = parseDeviceColors(contents)
	if err != nil || len(colors.Exact) != 0 || len(colors.Contains) != 0 {
		t.Fatal("reset should clear all device color rules")
	}
}
