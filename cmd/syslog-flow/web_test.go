package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestPagesAndPollsCarryIndexGeneration(t *testing.T) {
	previous := stateIndex
	t.Cleanup(func() { stateIndex = previous })
	now := appNow()
	stateIndex = &logIndex{generation: "current-generation", lastRefresh: now, files: make(map[string]*indexedFile)}
	day := now.Format("2006/01/02")
	for _, path := range []string{"/", "/statistics", "/day/" + day} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest("GET", path, nil)
		switch path {
		case "/":
			handleIndex(response, request)
		case "/statistics":
			handleOverview(response, request)
		default:
			handleDay(response, request)
		}
		if response.Code != 200 || !strings.Contains(response.Body.String(), `data-generation="current-generation"`) {
			t.Fatalf("%s: page missing generation (status %d)", path, response.Code)
		}
	}
	for _, path := range []string{
		"/?partial=logs&format=json",
		"/?partial=stats&format=json",
		"/day/" + day + "?partial=logs&format=json&since=1",
		"/day/" + day + "?partial=logs&format=json&before=0",
		"/api/overview",
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest("GET", path, nil)
		switch request.URL.Path {
		case "/":
			handleIndex(response, request)
		case "/api/overview":
			handleAPIOverview(response, request)
		default:
			handleDay(response, request)
		}
		var payload struct{ Generation string }
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatalf("%s: %v; body: %s", path, err, response.Body.String())
		}
		if response.Code != 200 || payload.Generation != "current-generation" {
			t.Fatalf("%s: generation = %q (status %d)", path, payload.Generation, response.Code)
		}
	}
}

func TestEmptyHomepageRendersLiveViewer(t *testing.T) {
	var output bytes.Buffer
	if err := page.Execute(&output, PageData{Live: true, RefreshURL: "/?partial=logs&format=json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `id="log-viewer"`) || !strings.Contains(output.String(), `data-refresh-url=`) {
		t.Fatal("empty homepage must render a viewer with polling enabled")
	}
}

func TestViewerLiveUpdates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is needed to run the viewer JavaScript regression checks")
	}
	command := exec.Command(node, "testdata/viewer_test.cjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("viewer checks: %v\n%s", err, output)
	}
}
