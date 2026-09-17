package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
)

func TestTrimHistoryReadPageHonorsByteBoundWithoutSkippingPrefix(t *testing.T) {
	page := model.HistoryReadPage{
		SchemaID: model.HistoryReadSchemaID, LogicalDialogID: "10000000-0000-4000-8000-000000000001",
		Entries: []model.HistoryTranscriptEntry{}, Facts: []model.HistoryExecutionFact{}, Receipts: []model.HistoryReceiptRevision{},
	}
	content := strings.Repeat("x", 65_536)
	for index := range model.HistoryMaximumPageSize {
		entryID := model.HistoryStableUUID("history-large-read-entry", fmt.Sprintf("%d", index))
		page.Entries = append(page.Entries, model.HistoryTranscriptEntry{
			EntryID: entryID, MessageID: entryID, LogicalDialogID: page.LogicalDialogID,
			Content:   json.RawMessage(fmt.Sprintf(`{"kind":"inline","content":%q}`, content)),
			CreatedAt: fmt.Sprintf("2026-09-17T00:%02d:%02dZ", index/60, index%60),
		})
	}
	trimmed, err := trimHistoryReadPage(&page)
	if err != nil || !trimmed || len(page.Entries) == 0 || len(page.Entries) >= model.HistoryMaximumPageSize {
		t.Fatalf("large read trim: entries=%d trimmed=%v err=%v", len(page.Entries), trimmed, err)
	}
	raw, err := json.Marshal(page)
	if err != nil || len(raw) > historyReadDataBudget {
		t.Fatalf("trimmed read bytes=%d err=%v", len(raw), err)
	}
	// Replacing JSON null with the maximum quoted cursor and adding the encoder
	// newline must still fit the end-to-end 8 MiB client boundary.
	if len(raw)-len("null")+historyMaximumCursorBytes+2+1 > model.HistoryMaximumPageBytes {
		t.Fatalf("trimmed page leaves no room for maximum cursor: bytes=%d", len(raw))
	}
	for index, entry := range page.Entries {
		want := model.HistoryStableUUID("history-large-read-entry", fmt.Sprintf("%d", index))
		if entry.EntryID != want {
			t.Fatalf("read prefix skipped at %d: got=%s want=%s", index, entry.EntryID, want)
		}
	}
}
