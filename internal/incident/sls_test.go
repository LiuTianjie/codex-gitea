package incident

import (
	"strings"
	"testing"
)

func TestParseSLSLogstoresSupportsMultipleSeparatorsAndDeduplicates(t *testing.T) {
	got := parseSLSLogstores(" function-log-prod-flat, taskiq-log-prod-flat；function-log-prod-flat\nextra ")
	want := []string{"function-log-prod-flat", "taskiq-log-prod-flat", "extra"}
	if len(got) != len(want) {
		t.Fatalf("parseSLSLogstores() = %#v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("parseSLSLogstores() = %#v, want %#v", got, want)
		}
	}
}

func TestFormatSLSLogsKeepsLogstoreSource(t *testing.T) {
	logs := formatSLSLogs([]map[string]string{{"__logstore": "function-log-prod-flat", "trace_id": "trace-1"}})
	if len(logs) != 1 || !strings.Contains(logs[0], `"__logstore":"function-log-prod-flat"`) {
		t.Fatalf("formatSLSLogs() = %#v", logs)
	}
}
