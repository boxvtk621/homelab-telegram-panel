package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateHistorySearchEntryAcceptsOnlySafeContentContract(t *testing.T) {
	entry := HistoryTranscriptEntry{
		EntryID: "10000000-0000-4000-8000-000000000001",
		Origin: HistoryOrigin{
			NodeID: "10000000-0000-4000-8000-000000000002", NodeDialogID: "10000000-0000-4000-8000-000000000003",
			AuthorEventID: "event:1",
		},
		LogicalDialogID:  "10000000-0000-4000-8000-000000000004",
		MessageID:        "10000000-0000-4000-8000-000000000005",
		Kind:             "message",
		Role:             "assistant",
		Content:          json.RawMessage(`{"kind":"inline","content":"safe","redaction":"none","truncated":false}`),
		CreatedAt:        "2026-09-18T00:00:00Z",
		ExecutionOrdinal: 1,
		ExecutionStatus:  "recorded",
	}
	if err := ValidateHistorySearchEntry(entry); err != nil || HistorySearchInlineText(entry) != "safe" {
		t.Fatalf("valid safe entry rejected: text=%q err=%v", HistorySearchInlineText(entry), err)
	}
	for _, raw := range []string{
		`{"kind":"inline","content":"unsafe missing classification"}`,
		`{"kind":"inline","content":"safe","redaction":"none","truncated":false,"native":{"path":"/private"}}`,
		`{"kind":"native","content":"provider payload"}`,
		`{"kind":"inline","content":"` + strings.Repeat("x", 64<<10+1) + `","redaction":"none","truncated":false}`,
	} {
		entry.Content = json.RawMessage(raw)
		if err := ValidateHistorySearchEntry(entry); err == nil {
			t.Fatalf("unsafe assistant content accepted: %.100s", raw)
		}
	}
	entry.Role = "user"
	entry.Content = json.RawMessage(`{"kind":"inline","content":"exact user text"}`)
	if err := ValidateHistorySearchEntry(entry); err != nil || HistorySearchInlineText(entry) != "exact user text" {
		t.Fatalf("valid user content rejected: err=%v", err)
	}
	entry.Content = json.RawMessage(`{"kind":"inline","content":"user","redaction":"none","truncated":false}`)
	if err := ValidateHistorySearchEntry(entry); err == nil {
		t.Fatal("assistant-shaped content accepted for user entry")
	}
}
