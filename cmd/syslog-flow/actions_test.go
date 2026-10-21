package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testActionRule() actionRule {
	return actionRule{ID: "disk-alert", Name: "Disk alert", Enabled: true, Severities: []string{"crit", "err"}, CooldownSeconds: 300, Type: "webhook", URL: "https://example.invalid/hook"}
}

func configuredActions(t *testing.T, rule actionRule) *actionEngine {
	t.Helper()
	engine := newActionEngine(filepath.Join(t.TempDir(), "actions.json"))
	if err := engine.saveLocked(actionsConfig{Rules: []actionRule{rule}}); err != nil {
		t.Fatal(err)
	}
	return engine
}

func actionLine(at time.Time, device, level, message string) string {
	stamp := at.Format(time.RFC3339)
	return stamp + " | " + stamp + " " + device + " " + level + " app: " + message + "\n"
}

func appendActionLog(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString(text); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestActionsValidation(t *testing.T) {
	rule := testActionRule()
	data, _ := json.Marshal(actionsConfig{Rules: []actionRule{rule}})
	if _, err := parseActionsConfig(data); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"null", `{}`, `{"rules":null}`, `{"rules":123}`} {
		if _, err := parseActionsConfig([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*actionRule)
	}{
		{"no severity", func(r *actionRule) { r.Severities = nil }},
		{"unknown severity", func(r *actionRule) { r.Severities = []string{"fatal"} }},
		{"negative cooldown", func(r *actionRule) { r.CooldownSeconds = -1 }},
		{"local file", func(r *actionRule) { r.URL = "file:///etc/passwd" }},
		{"embedded credentials", func(r *actionRule) { r.URL = "http://user:secret@example.invalid" }},
		{"header injection", func(r *actionRule) { r.Headers = map[string]string{"Authorization": "secret\r\nX-Foo: bar"} }},
		{"bad header name", func(r *actionRule) { r.Headers = map[string]string{"X bad": "foo"} }},
		{"bad service", func(r *actionRule) { r.Type = "shoutrrr"; r.URL = "unknown-service://secret" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rule
			tc.change(&r)
			data, _ := json.Marshal(actionsConfig{Rules: []actionRule{r}})
			if _, err := parseActionsConfig(data); err == nil {
				t.Fatal("accepted invalid rule")
			}
		})
	}
	rule.Type = "shoutrrr"
	rule.URL = "logger://"
	data, _ = json.Marshal(actionsConfig{Rules: []actionRule{rule}})
	if _, err := parseActionsConfig(data); err != nil {
		t.Fatalf("valid Shoutrrr URL: %v", err)
	}
}

func TestActionsConditionsCooldownAndQueueLimit(t *testing.T) {
	rule := testActionRule()
	rule.Contains = "DISK"
	engine := configuredActions(t, rule)
	now := appNow()
	engine.consider(actionLine(now, "router", "info", "disk error"), now)
	engine.consider(actionLine(now, "router", "crit", "network error"), now)
	if len(engine.queue) != 0 {
		t.Fatal("nonmatching messages were queued")
	}
	engine.consider(actionLine(now, "router", "crit", "disk error"), now)
	engine.consider(actionLine(now, "router", "crit", "disk error again"), now.Add(time.Second))
	engine.consider(actionLine(now, "nas", "err", "Disk error"), now.Add(time.Second))
	if len(engine.queue) != 2 {
		t.Fatalf("queued %d jobs, want one per device", len(engine.queue))
	}
	engine.consider(actionLine(now, "router", "crit", "disk error"), now.Add(300*time.Second))
	if len(engine.queue) != 3 {
		t.Fatal("cooldown did not expire")
	}
	rule.Device = "router"
	rule.CooldownSeconds = 0
	engine = configuredActions(t, rule)
	engine.consider(actionLine(now, "nas", "crit", "disk error"), now)
	if len(engine.queue) != 0 {
		t.Fatal("device condition ignored")
	}
	for i := 0; i < 65; i++ {
		engine.consider(actionLine(now, "router", "crit", "disk error"), now)
	}
	if len(engine.queue) != 64 || engine.statuses[rule.ID].Dropped != 1 {
		t.Fatal("queue must be bounded and report dropped deliveries")
	}
}

func TestActionReaderStartupPartialWritesAndMidnight(t *testing.T) {
	rule := testActionRule()
	rule.CooldownSeconds = 0
	engine := configuredActions(t, rule)
	root := t.TempDir()
	now := appNow()
	before := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, appLocation)
	oldPath := filepath.Join(root, before.Format("2006/01/02"), "router.log")
	appendActionLog(t, oldPath, actionLine(before, "router", "crit", "historical")+"unfinished startup entry")
	reader := &actionReader{root: root, cursors: make(map[string]*actionFileCursor), engine: engine}
	if err := reader.poll(before, true); err != nil {
		t.Fatal(err)
	}
	appendActionLog(t, oldPath, " remainder\n"+actionLine(before, "router", "crit", "new before midnight"))
	if err := reader.poll(before, false); err != nil {
		t.Fatal(err)
	}
	if len(engine.queue) != 1 {
		t.Fatalf("startup history was replayed or new entry lost: %d", len(engine.queue))
	}
	after := before.Add(2 * time.Second)
	newPath := filepath.Join(root, after.Format("2006/01/02"), "router.log")
	line := actionLine(after, "router", "err", "new after midnight")
	appendActionLog(t, oldPath, actionLine(after, "router", "crit", "late write to yesterday"))
	appendActionLog(t, newPath, line[:len(line)-3])
	if err := reader.poll(after, false); err != nil {
		t.Fatal(err)
	}
	if len(engine.queue) != 2 {
		t.Fatal("partial entry emitted or yesterday's late write lost")
	}
	appendActionLog(t, newPath, line[len(line)-3:])
	if err := reader.poll(after, false); err != nil {
		t.Fatal(err)
	}
	if len(engine.queue) != 3 {
		t.Fatal("completed entry not emitted")
	}
	if err := reader.poll(after, false); err != nil {
		t.Fatal(err)
	}
	if len(engine.queue) != 3 {
		t.Fatal("entries replayed on repeated poll")
	}
	// A fresh reader models a restart and must baseline both days again.
	restarted := &actionReader{root: root, cursors: make(map[string]*actionFileCursor), engine: engine}
	if err := restarted.poll(after, true); err != nil {
		t.Fatal(err)
	}
	if len(engine.queue) != 3 {
		t.Fatal("restart replayed historical entries")
	}
	// Replacing an active file starts reading the new file at the beginning.
	if err := os.Rename(newPath, newPath+".old"); err != nil {
		t.Fatal(err)
	}
	appendActionLog(t, newPath, actionLine(after, "router", "err", "replacement"))
	if err := reader.poll(after, false); err != nil {
		t.Fatal(err)
	}
	if len(engine.queue) != 4 {
		t.Fatal("replacement file not monitored")
	}
}

func TestActionsDeliverWithoutBrowser(t *testing.T) {
	received := make(chan actionEvent, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-secret" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("webhook method or headers incorrect")
		}
		var event actionEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
		}
		received <- event
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	rule := testActionRule()
	rule.URL = server.URL
	rule.Headers = map[string]string{"Authorization": "Bearer test-secret"}
	engine := configuredActions(t, rule)
	root := t.TempDir()
	now := appNow()
	path := filepath.Join(root, now.Format("2006/01/02"), "router.log")
	appendActionLog(t, path, actionLine(now, "router", "crit", "old"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := engine.start(ctx, root); err != nil {
		t.Fatal(err)
	}
	appendActionLog(t, path, actionLine(now, "router", "crit", "disk unavailable"))
	select {
	case event := <-received:
		if event.Device != "router" || event.Severity != "crit" || event.Rule != rule.Name || event.Test || !strings.Contains(event.Message, "disk unavailable") {
			t.Fatalf("event: %#v", event)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("background delivery did not run")
	}
	if err := engine.test(rule.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-received:
		if !event.Test {
			t.Fatal("test notification not marked")
		}
	case <-time.After(time.Second):
		t.Fatal("test action not delivered")
	}
}

func TestActionDeliveryFailureTimeoutAndShoutrrr(t *testing.T) {
	secret := "do-not-leak-this"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, secret, http.StatusUnauthorized) }))
	defer server.Close()
	rule := testActionRule()
	rule.URL = server.URL + "/?token=" + secret
	err := sendAction(context.Background(), rule, actionEvent{})
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe or missing error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sendAction(ctx, rule, actionEvent{}); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancellation: %v", err)
	}
	rule.Type = "shoutrrr"
	rule.URL = "logger://"
	if err := sendAction(context.Background(), rule, actionEvent{Device: "router", Severity: "crit", Timestamp: appNow(), Message: "test"}); err != nil {
		t.Fatal(err)
	}
}

func TestActionsSettingsLifecycleAndSecretFile(t *testing.T) {
	previous := actions
	defer func() { actions = previous }()
	actions = newActionEngine(filepath.Join(t.TempDir(), "actions.json"))
	post := func(values url.Values) *httptest.ResponseRecorder {
		t.Helper()
		values.Set("section", "actions")
		values.Set("csrf", settingsCSRFToken)
		request := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Accept", "application/json")
		response := httptest.NewRecorder()
		handleSettings(response, request)
		return response
	}
	values := url.Values{"action": {"save"}, "name": {"Disk alert"}, "enabled": {"on"}, "severities": {"crit", "err"}, "cooldown_seconds": {"300"}, "type": {"webhook"}, "url": {"https://example.invalid/hook"}, "headers": {"Authorization: Bearer secret"}, "payload_mode": {"custom"}, "payload": {`{"entity_id":"light.kitchen_pendants"}`}}
	if response := post(values); response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	if len(actions.config.Rules) != 1 {
		t.Fatal("rule not saved")
	}
	id := actions.config.Rules[0].ID
	if actions.config.Rules[0].Payload == nil || *actions.config.Rules[0].Payload != values.Get("payload") {
		t.Fatal("custom payload was not saved")
	}
	info, err := os.Stat(actions.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode: %v", info.Mode())
	}
	// Edit renders the eight checkboxes and safely escapes user-provided text.
	response := httptest.NewRecorder()
	handleSettings(response, httptest.NewRequest(http.MethodGet, "/settings?section=actions&edit="+id, nil))
	body := response.Body.String()
	if response.Code != http.StatusOK || strings.Count(body, `type="checkbox" name="severities"`) != 8 || !strings.Contains(body, `value="crit" checked`) || !strings.Contains(body, `value="err" checked`) || !strings.Contains(body, `name="id" value="`+id+`"`) {
		t.Fatalf("invalid Actions form: %s", body)
	}
	values.Set("id", id)
	values.Set("name", "Edited alert")
	values.Set("payload_mode", "log")
	if response := post(values); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if len(actions.config.Rules) != 1 || actions.config.Rules[0].Name != "Edited alert" {
		t.Fatal("edit created a duplicate")
	}
	if actions.config.Rules[0].Payload != nil {
		t.Fatal("log-event mode must clear the custom payload")
	}
	if response := post(url.Values{"action": {"toggle"}, "id": {id}}); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if actions.config.Rules[0].Enabled {
		t.Fatal("rule not disabled")
	}
	if response := post(url.Values{"action": {"test"}, "id": {id}}); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if len(actions.queue) != 1 {
		t.Fatal("test must work for a disabled rule")
	}
	if response := post(url.Values{"action": {"delete"}, "id": {id}}); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if len(actions.config.Rules) != 0 {
		t.Fatal("rule not deleted")
	}
	// Test/save mutations retain the existing CSRF protection.
	request := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader("section=actions&action=test&id="+id))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	handleSettings(response, request)
	if response.Code != 400 {
		t.Fatal("missing CSRF token accepted")
	}
	disk, err := os.Open(actions.path)
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	contents, _ := io.ReadAll(disk)
	if strings.Contains(string(contents), "secret") {
		t.Fatal("deleted action credentials remained in config")
	}
}

func TestActionsInvalidReloadKeepsLastGoodConfiguration(t *testing.T) {
	engine := configuredActions(t, testActionRule())
	if err := os.WriteFile(engine.path, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := engine.reload(); err == nil {
		t.Fatal("invalid config accepted")
	}
	if len(engine.config.Rules) != 1 || engine.configError == "" {
		t.Fatal("must retain last good rules and expose reload error")
	}
}

func TestActionsReloadRecoversUnchangedFile(t *testing.T) {
	engine := configuredActions(t, testActionRule())
	moved := engine.path + ".moved"
	if err := os.Rename(engine.path, moved); err != nil {
		t.Fatal(err)
	}
	if err := engine.reload(); err == nil || engine.configError == "" {
		t.Fatal("missing configuration should report an error")
	}
	// Restore the same inode, size and modification time as the last good read.
	if err := os.Rename(moved, engine.path); err != nil {
		t.Fatal(err)
	}
	if err := engine.reload(); err != nil {
		t.Fatal(err)
	}
	if engine.configError != "" || len(engine.config.Rules) != 1 {
		t.Fatal("restored configuration must clear the error and retain its rules")
	}
}

func TestActionReaderSkipsOversizedLines(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(strconv.FormatBool(partial), func(t *testing.T) {
			rule := testActionRule()
			rule.CooldownSeconds = 0
			engine := configuredActions(t, rule)
			root := t.TempDir()
			now := appNow()
			path := filepath.Join(root, now.Format("2006/01/02"), "router.log")
			reader := &actionReader{root: root, cursors: make(map[string]*actionFileCursor), engine: engine}
			oversized := strings.Repeat("x", maxActionLineSize+100)
			if !partial {
				oversized += "\n" + actionLine(now, "router", "crit", "first valid entry")
			}
			appendActionLog(t, path, oversized)
			if err := reader.poll(now, false); err == nil || !strings.Contains(err.Error(), "exceeding 1 MiB") {
				t.Fatalf("oversized line should be reported: %v", err)
			}
			if partial {
				if reader.cursors[path].offset != int64(len(oversized)) {
					t.Fatal("oversized partial line was not consumed")
				}
				// A valid-looking suffix still belongs to the discarded oversized record.
				appendActionLog(t, path, actionLine(now, "router", "crit", "discard this suffix"))
			}
			appendActionLog(t, path, actionLine(now, "router", "crit", "next valid entry"))
			if err := reader.poll(now, false); err != nil {
				t.Fatal(err)
			}
			expected := []string{"app: first valid entry", "app: next valid entry"}
			if partial {
				expected = expected[1:]
			}
			for _, message := range expected {
				select {
				case job := <-engine.queue:
					if job.event.Message != message {
						t.Fatalf("unexpected message: %q", job.event.Message)
					}
				default:
					t.Fatal("valid entry after oversized line was lost")
				}
			}
			if err := reader.poll(now, false); err != nil || len(engine.queue) != 0 {
				t.Fatalf("unchanged file should neither replay entries nor repeat its error: %v", err)
			}
		})
	}
}

func TestActionsDeliverInQueueOrder(t *testing.T) {
	firstStarted := make(chan struct{}, 1)
	secondStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	applied := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event actionEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(event.Message, "first") {
			firstStarted <- struct{}{}
			<-releaseFirst
		} else {
			secondStarted <- struct{}{}
		}
		applied <- event.Message
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	// Always release the handler before shutting down the test server.
	defer func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
	}()
	rule := testActionRule()
	rule.URL = server.URL
	rule.CooldownSeconds = 0
	engine := configuredActions(t, rule)
	now := appNow()
	engine.consider(actionLine(now, "router", "crit", "first"), now)
	engine.consider(actionLine(now, "router", "crit", "second"), now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := engine.start(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first delivery did not start")
	}
	select {
	case <-secondStarted:
		t.Error("second delivery started before the first completed")
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseFirst)
	for _, expected := range []string{"first", "second"} {
		select {
		case message := <-applied:
			if !strings.Contains(message, expected) {
				t.Fatalf("expected %s delivery, got %q", expected, message)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("queued delivery did not complete")
		}
	}
}

func TestActionsEditorDataEscapesMarkup(t *testing.T) {
	previous := actions
	t.Cleanup(func() { actions = previous })
	rule := testActionRule()
	rule.Name = `</script><script>alert("unsafe")</script>`
	rule.Contains = `quoted "message" <tag>`
	actions = configuredActions(t, rule)
	response := httptest.NewRecorder()
	handleSettings(response, httptest.NewRequest(http.MethodGet, "/settings?section=actions", nil))
	body := response.Body.String()
	if strings.Contains(body, rule.Name) {
		t.Fatal("rule name can break out of the editor's JSON script")
	}
	_, script, found := strings.Cut(body, `<script id="action-rules" type="application/json">`)
	if !found {
		t.Fatal("missing editor data")
	}
	data, _, found := strings.Cut(script, "</script>")
	var rules []actionRule
	if !found || json.Unmarshal([]byte(data), &rules) != nil || len(rules) != 1 {
		t.Fatal("editor data must remain valid JSON")
	}
	if rules[0].Name != rule.Name || rules[0].Contains != rule.Contains {
		t.Fatal("escaping changed the editable rule values")
	}
}

func TestActionsSaveCancelsQueuedDeliveries(t *testing.T) {
	received := make(chan actionEvent, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event actionEvent
		_ = json.NewDecoder(r.Body).Decode(&event)
		received <- event
		w.WriteHeader(204)
	}))
	defer server.Close()
	rule := testActionRule()
	rule.URL = server.URL
	engine := configuredActions(t, rule)
	now := appNow()
	engine.consider(actionLine(now, "router", "crit", "queued before disable"), now)
	if len(engine.queue) != 1 {
		t.Fatal("missing queued job")
	}
	rule.Enabled = false
	if err := engine.saveLocked(actionsConfig{Rules: []actionRule{rule}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go engine.worker(ctx)
	if err := engine.test(rule.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-received:
		if !event.Test {
			t.Fatal("stale automatic delivery sent after disable")
		}
	case <-time.After(time.Second):
		t.Fatal("new test job was not delivered")
	}
}

func BenchmarkActionsMatch(b *testing.B) {
	engine := newActionEngine("")
	for i := 0; i < 10; i++ {
		rule := testActionRule()
		rule.ID = strconv.Itoa(i)
		rule.Contains = "disk"
		engine.config.Rules = append(engine.config.Rules, rule)
	}
	now := appNow()
	line := actionLine(now, "router", "crit", "disk unavailable")
	engine.consider(line, now)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		engine.consider(line, now)
	}
}

func TestActionCustomPayloadDelivery(t *testing.T) {
	jsonBody := `{"entity_id":"light.kitchen_pendants"}`
	textBody := "Kitchen alert\nLiteral text: {{message}}"
	emptyBody := ""
	for _, tc := range []struct {
		name, kind string
		payload    *string
		want       string
	}{
		{"webhook custom JSON", "webhook", &jsonBody, jsonBody},
		{"webhook empty body", "webhook", &emptyBody, ""},
		{"webhook default event", "webhook", nil, ""},
		{"Shoutrrr custom message", "shoutrrr", &textBody, textBody},
		{"Shoutrrr default message", "shoutrrr", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				received <- string(body)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			rule := testActionRule()
			rule.Type = tc.kind
			rule.Payload = tc.payload
			rule.URL = server.URL
			if rule.Type == "shoutrrr" {
				rule.URL = "generic://" + strings.TrimPrefix(server.URL, "http://") + "?disabletls=yes"
			}
			event := actionEvent{Rule: rule.Name, Device: "router", Severity: "crit", Timestamp: appNow(), Message: "original log details", Test: true}
			if err := sendAction(context.Background(), rule, event); err != nil {
				t.Fatal(err)
			}
			body := <-received
			if tc.payload != nil {
				if body != tc.want {
					t.Fatalf("body = %q, want %q", body, tc.want)
				}
				if strings.Contains(body, event.Message) || strings.Contains(body, `"test":`) {
					t.Fatal("custom payload must not include event metadata")
				}
			} else if rule.Type == "webhook" {
				var actual actionEvent
				if err := json.Unmarshal([]byte(body), &actual); err != nil || actual.Device != event.Device || actual.Message != event.Message || !actual.Test {
					t.Fatalf("default event changed: %s; %v", body, err)
				}
			} else if !strings.Contains(body, event.Message) || !strings.Contains(body, "[CRIT] router") {
				t.Fatalf("default notification changed: %q", body)
			}
		})
	}
}

func TestActionPayloadValidationAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		name, kind, payload string
		valid               bool
	}{
		{"webhook JSON", "webhook", `{"entity_id":"light.kitchen_pendants"}`, true},
		{"webhook empty", "webhook", "", true},
		{"webhook invalid JSON", "webhook", `{"entity_id":`, false},
		{"Shoutrrr text", "shoutrrr", "Kitchen alert", true},
		{"Shoutrrr empty", "shoutrrr", "", false},
		{"too large", "webhook", strings.Repeat(" ", 65537), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rule := testActionRule()
			rule.Type = tc.kind
			rule.Payload = &tc.payload
			if rule.Type == "shoutrrr" {
				rule.URL = "logger://"
			}
			data, _ := json.Marshal(actionsConfig{Rules: []actionRule{rule}})
			_, err := parseActionsConfig(data)
			if (err == nil) != tc.valid {
				t.Fatalf("valid = %t; error: %v", tc.valid, err)
			}
			if !tc.valid {
				return
			}
			engine := configuredActions(t, rule)
			reloaded := newActionEngine(engine.path)
			if err := reloaded.reload(); err != nil {
				t.Fatal(err)
			}
			actual := reloaded.config.Rules[0].Payload
			if actual == nil || *actual != tc.payload {
				t.Fatalf("payload not preserved: %v", actual)
			}
		})
	}
}
