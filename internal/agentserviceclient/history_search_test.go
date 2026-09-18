package agentserviceclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/historyreplica"
	"github.com/boxvtk621/homelab-telegram-panel/internal/historysearch"
)

const (
	searchDialogID     = "91000000-0000-4000-8000-000000000001"
	searchEntryID      = "91000000-0000-4000-8000-000000000002"
	searchNodeID       = "91000000-0000-4000-8000-000000000003"
	searchNodeDialogID = "91000000-0000-4000-8000-000000000004"
)

func testSearchPage() historysearch.Page {
	return historysearch.Page{
		SchemaID: historysearch.SchemaID,
		Query:    historysearch.Query{Q: "кириллица exact-id", NodeID: searchNodeID},
		Items: []historysearch.Item{{
			LogicalDialogID: searchDialogID, EntryID: searchEntryID, NodeID: searchNodeID, NodeDialogID: searchNodeDialogID,
			Title: "Архив", Role: "assistant", Kind: "message", CreatedAt: "2026-09-18T08:00:00Z",
			Rank: 0.75, Snippet: "безопасный результат",
		}},
		TotalCount: 1, ObservedAt: "2026-09-18T08:00:01Z", LagMillis: 1, Incomplete: true,
	}
}

func TestHistorySearchClientScopesReadsAndPreservesGone(t *testing.T) {
	query := historysearch.Query{Q: "КИРИЛЛИЦА   exact-id", NodeID: searchNodeID}
	calls := 0
	client := &Client{http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodGet || request.URL.Path != "/internal/v1/history-search" ||
			request.Header.Get(OwnerHeader) != "owner-1" || request.URL.Query().Get("q") != query.Q ||
			request.URL.Query().Get("nodeId") != searchNodeID || request.URL.Query().Get("limit") != "50" {
			t.Fatalf("search request=%s %s headers=%v", request.Method, request.URL.String(), request.Header)
		}
		if calls == 2 {
			return response(http.StatusGone, map[string]any{"error": map[string]any{
				"code": "search_snapshot_expired", "message": "safe", "retryable": false,
			}}), nil
		}
		return response(http.StatusOK, testSearchPage()), nil
	})}}
	page, err := client.SearchHistory(context.Background(), "owner-1", query, 50, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].EntryID != searchEntryID || !page.Incomplete {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	_, err = client.SearchHistory(context.Background(), "owner-1", query, 50, "snapshot.next")
	var fault *Fault
	if !errors.As(err, &fault) || fault.Status != http.StatusGone || fault.Code != "search_snapshot_expired" {
		t.Fatalf("expired fault=%#v err=%v", fault, err)
	}
}

func TestHistoryEntryClientRequiresCanonicalSafeContent(t *testing.T) {
	entry := historyreplica.TranscriptEntry{
		EntryID: searchEntryID,
		Origin: historyreplica.Origin{
			NodeID: searchNodeID, NodeDialogID: searchNodeDialogID, AuthorEventID: "event:7",
		},
		LogicalDialogID: searchDialogID, MessageID: searchEntryID, AttemptID: "91000000-0000-4000-8000-000000000005",
		Kind: "message", Role: "assistant",
		Content:   json.RawMessage(`{"kind":"inline","content":"safe","redaction":"none","truncated":false}`),
		CreatedAt: "2026-09-18T08:00:00Z", ProfileHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SourceGeneration: 1, ExecutionOrdinal: 1, ExecutionStatus: "recorded",
	}
	client := &Client{http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/internal/v1/history-replica/dialogs/"+searchDialogID+"/entries/"+searchEntryID {
			t.Fatalf("entry path=%s", request.URL.Path)
		}
		return response(http.StatusOK, HistoryEntryLookup{
			SchemaID: historysearch.EntryLookupSchemaID, Entry: entry, ObservedAt: "2026-09-18T08:00:01Z", LagMillis: 1,
		}), nil
	})}}
	result, err := client.HistoryEntryByID(context.Background(), "owner-1", searchDialogID, searchEntryID)
	if err != nil || result.Entry.EntryID != searchEntryID {
		t.Fatalf("entry=%+v err=%v", result, err)
	}

	entry.Content = json.RawMessage(`{"kind":"native_path","content":"/private/db.sqlite"}`)
	poisoned := &Client{http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, HistoryEntryLookup{
			SchemaID: historysearch.EntryLookupSchemaID, Entry: entry, ObservedAt: "2026-09-18T08:00:01Z",
		}), nil
	})}}
	if _, err := poisoned.HistoryEntryByID(context.Background(), "owner-1", searchDialogID, searchEntryID); err == nil {
		t.Fatal("unsafe entry content crossed the private client")
	}
}

func TestHistoryMetadataUsesWorkerScopedPut(t *testing.T) {
	workerToken := "worker-test-token"
	input := historysearch.DialogMetadata{
		SchemaID: historysearch.MetadataSchemaID, NodeID: searchNodeID, NodeDialogID: searchNodeDialogID,
		BindingGeneration: 1, Title: "Диалог", Archived: false, UpdatedAt: "2026-09-18T08:00:00Z",
	}
	client := &Client{workerToken: workerToken, http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPut || request.URL.Path != "/internal/v1/history-search/dialogs/"+searchDialogID+"/metadata" ||
			request.Header.Get(OwnerHeader) != "owner-1" || request.Header.Get("X-Agent-Service-Worker-Token") != workerToken {
			t.Fatalf("metadata request=%s %s headers=%v", request.Method, request.URL.Path, request.Header)
		}
		result := input
		result.LogicalDialogID = searchDialogID
		return response(http.StatusOK, result), nil
	})}}
	result, err := client.UpsertHistoryDialogMetadata(context.Background(), "owner-1", searchDialogID, input)
	if err != nil || result.Title != input.Title || result.LogicalDialogID != searchDialogID {
		t.Fatalf("metadata=%+v err=%v", result, err)
	}
}
