package main

import (
	"bytes"
	"fmt"
	"html"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSearchLinkOpensExactLineWithDuplicateTimestamps(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, appLocation)
	day, file := "2026/10/20", "router.log"
	var contents strings.Builder
	for i := 1; i <= 1200; i++ {
		contents.WriteString(actionLine(at, "router", "err", fmt.Sprintf("entry %04d", i)))
	}
	path := filepath.Join(root, day, file)
	appendActionLog(t, path, contents.String())
	results, err := searchDays(root, []Day{{Name: day, Files: []LogFile{{Name: file}}}}, logFilter{Query: "entry 0700"}, "")
	if err != nil || len(results.lines) != 1 || len(results.links) != 1 {
		t.Fatalf("search did not retain a source link: %+v, %v", results, err)
	}
	link, err := url.Parse(results.links[0])
	if err != nil || link.Path != "/day/"+day || link.Fragment != "log-target" || link.Query().Get("file") != file {
		t.Fatalf("bad source link: %s, %v", results.links[0], err)
	}
	target, err := parseLogTarget(link.Query())
	if err != nil || target != 700 {
		t.Fatalf("search must retain the unfiltered physical line: %d, %v", target, err)
	}
	lines, start, total, err := readLogContext(root, day, file, target, link.Query().Get("match"))
	if err != nil || total != 1200 || len(lines) != dayChunkSize || lines[target-start-1] != results.lines[0] {
		t.Fatalf("incorrect context window: start=%d, total=%d, lines=%d, err=%v", start, total, len(lines), err)
	}
	data := PageData{Selected: day, File: file, Lines: lines, TargetIndex: target - start, ContextView: true}
	var output bytes.Buffer
	if err := page.Execute(&output, data); err != nil {
		t.Fatal(err)
	}
	body := output.String()
	if strings.Count(body, `id="log-target"`) != 1 || !strings.Contains(body, `id="log-target" data-raw="`+html.EscapeString(results.lines[0])+`"`) || strings.Contains(body, `type="button" data-live-toggle`) {
		t.Fatal("context view must highlight exactly one entry and pause live updates")
	}
	appendActionLog(t, path, actionLine(at, "router", "err", "later entry"))
	if _, _, total, err = readLogContext(root, day, file, target, link.Query().Get("match")); err != nil || total != 1201 {
		t.Fatalf("append invalidated an existing source link: %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat(actionLine(at, "router", "err", "different entry"), 1200)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readLogContext(root, day, file, target, link.Query().Get("match")); err == nil || !strings.Contains(err.Error(), "no longer available") {
		t.Fatal("changed source file must report an unavailable entry")
	}
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readLogContext(root, day, file, target, link.Query().Get("match")); err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Fatal("deleted source day must report an unavailable entry")
	}
}

func TestLogContextBoundariesAndLinkValidation(t *testing.T) {
	root := t.TempDir()
	day, file := "2026/10/20", "router.log"
	raw := strings.TrimSuffix(actionLine(appNow(), "router", "err", "identical entry"), "\n")
	appendActionLog(t, filepath.Join(root, day, file), strings.Repeat(raw+"\n", 600))
	for _, target := range []int{1, 600} {
		lines, start, total, err := readLogContext(root, day, file, target, logRecordHash(raw))
		if err != nil || total != 600 || target-start < 1 || target-start > len(lines) {
			t.Fatalf("boundary target %d not present in its window: %v", target, err)
		}
	}
	if _, _, _, err := readLogContext(root, day, file, 601, logRecordHash(raw)); err == nil {
		t.Fatal("out-of-range source line accepted")
	}
	for _, values := range []url.Values{
		{"line": {"0"}, "file": {file}, "match": {logRecordHash(raw)}},
		{"line": {"-1"}, "file": {file}, "match": {logRecordHash(raw)}},
		{"line": {"9999999999999999999999999"}, "file": {file}, "match": {logRecordHash(raw)}},
		{"line": {"1"}, "file": {"../router.log"}, "match": {logRecordHash(raw)}},
		{"line": {"1"}, "file": {file}, "match": {"invalid"}},
	} {
		if _, err := parseLogTarget(values); err == nil {
			t.Fatalf("accepted invalid source link: %v", values)
		}
	}
	request := httptest.NewRequest("GET", "/day/"+day+"?file=router.log&line=1&match="+logRecordHash(raw), nil)
	link, err := url.Parse(logRefreshURL(request))
	if err != nil || link.Query().Has("line") || link.Query().Has("match") || link.Query().Get("file") != file {
		t.Fatal("older pagination must retain the device without reapplying the context target")
	}
}

func TestGlobalResultsRenderTimestampLinks(t *testing.T) {
	raw := strings.TrimSuffix(actionLine(appNow(), "router", "err", "search result"), "\n")
	record := logRecord{line: "2026/10/20/router.log  " + raw, lineNumber: 42}
	results := searchRecordResults([]logRecord{record}, false)
	var output bytes.Buffer
	if err := page.Execute(&output, PageData{Global: true, Lines: results.lines, SearchLinks: results.links}); err != nil {
		t.Fatal(err)
	}
	body := html.UnescapeString(output.String())
	if !strings.Contains(body, `class="log-timestamp" href="`+results.links[0]+`"`) || !strings.Contains(body, `line=42`) {
		t.Fatal("global search timestamp must link to its original device file position")
	}
}
