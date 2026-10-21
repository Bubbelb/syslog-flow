package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func logRecordHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func searchRecordResults(records []logRecord, limited bool) searchResults {
	results := searchResults{lines: recordLines(records), limited: limited}
	for _, record := range records {
		label, raw, _ := strings.Cut(record.line, "  ")
		day, file := filepath.ToSlash(filepath.Dir(label)), filepath.Base(label)
		values := url.Values{"file": {file}, "line": {strconv.Itoa(record.lineNumber)}, "match": {logRecordHash(raw)}}
		results.links = append(results.links, "/day/"+day+"?"+values.Encode()+"#log-target")
	}
	return results
}

func (data PageData) SearchLink(index int) string {
	if index < len(data.SearchLinks) {
		return data.SearchLinks[index]
	}
	return ""
}

func (data PageData) IsTarget(index int) bool {
	return data.TargetIndex > 0 && index == data.TargetIndex-1
}

func parseLogTarget(values url.Values) (int, error) {
	raw := values.Get("line")
	if raw == "" {
		return 0, nil
	}
	line, err := strconv.Atoi(raw)
	checksum, hashErr := hex.DecodeString(values.Get("match"))
	if err != nil || line < 1 || hashErr != nil || len(checksum) != sha256.Size || !validName(values.Get("file")) {
		return 0, fmt.Errorf("invalid log entry link")
	}
	return line, nil
}

// Retain one window around the physical line, even when timestamps are duplicated.
func readLogContext(root, day, file string, target int, checksum string) ([]string, int, int, error) {
	opened, err := os.Open(filepath.Join(root, day, file))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, 0, fmt.Errorf("this log entry is no longer available; its day or device file may have been deleted")
		}
		return nil, 0, 0, err
	}
	defer opened.Close()
	start := max(0, target-1-dayChunkSize/2)
	lines := make([]string, 0, dayChunkSize)
	total := 0
	found := false
	scanner := bufio.NewScanner(opened)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		total++
		if total == target {
			found = logRecordHash(scanner.Text()) == strings.ToLower(checksum)
		}
		if total > start && total-start <= dayChunkSize {
			lines = append(lines, day+"/"+file+"  "+scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, total, err
	}
	if !found {
		return nil, 0, total, fmt.Errorf("this log entry is no longer available at its original position; the file may have changed")
	}
	return lines, start, total, nil
}
