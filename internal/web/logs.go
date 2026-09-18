package web

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// formatLogLine turns a JSON object into a compact log entry. Non-object JSON
// and ordinary text pass through unchanged. Formatting stays on one line so
// the dashboard's tail limit counts entries, not expanded JSON fields.
func formatLogLine(line string) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &fields); err != nil || len(fields) == 0 {
		return line
	}

	var parts []string
	for _, aliases := range [][]string{
		{"time", "timestamp", "ts"},
		{"level", "severity"},
		{"msg", "message"},
	} {
		for _, key := range aliases {
			var value string
			if err := json.Unmarshal(fields[key], &value); err != nil || value == "" {
				continue
			}
			if key == "level" || key == "severity" {
				value = strings.ToUpper(value)
			}
			if strings.ContainsFunc(value, unicode.IsControl) {
				value = strconv.Quote(value)
			}
			parts = append(parts, value)
			delete(fields, key)
			break
		}
	}

	// Stable ordering makes entries easier to scan. RawMessage preserves large
	// integers and timestamp precision, including inside nested objects.
	keys := slices.Sorted(maps.Keys(fields))
	var value bytes.Buffer
	for _, key := range keys {
		value.Reset()
		_ = json.Compact(&value, fields[key]) // Already validated by Unmarshal.
		label := key
		if label == "" || strings.ContainsFunc(label, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r) || r == '=' || r == '"' || r == '\\'
		}) {
			label = strconv.Quote(label)
		}
		parts = append(parts, label+"="+value.String())
	}
	return strings.Join(parts, " ")
}
