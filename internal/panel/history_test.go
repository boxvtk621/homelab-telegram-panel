package panel

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/agentserviceclient"
	"github.com/boxvtk621/homelab-telegram-panel/internal/historyreplica"
	"github.com/boxvtk621/homelab-telegram-panel/internal/historysearch"
)

const (
	historyLogicalID = "10000000-0000-4000-8000-000000000001"
	historyNodeID    = "10000000-0000-4000-8000-000000000002"
	historyCommandID = "10000000-0000-4000-8000-000000000003"
)

type fakeHistoryBackend struct {
	owner, logical, cursor string
	limit                  int
	read                   historyreplica.ReadPage
	search                 historysearch.Page
	entry                  agentserviceclient.HistoryEntryLookup
	searchQuery            historysearch.Query
	receipt                historyreplica.ReceiptLookup
	err                    error
}

type blockingHistoryBackend struct {
	fakeHistoryBackend
	entered chan struct{}
	release chan struct{}
}

func (b *blockingHistoryBackend) SearchHistory(ctx context.Context, owner string, query historysearch.Query, limit int, cursor string) (historysearch.Page, error) {
	close(b.entered)
	select {
	case <-b.release:
		return b.fakeHistoryBackend.SearchHistory(ctx, owner, query, limit, cursor)
	case <-ctx.Done():
		return historysearch.Page{}, ctx.Err()
	}
}

func (f *fakeHistoryBackend) SearchHistory(_ context.Context, owner string, query historysearch.Query, limit int, cursor string) (historysearch.Page, error) {
	f.owner, f.searchQuery, f.limit, f.cursor = owner, query, limit, cursor
	return f.search, f.err
}

func (f *fakeHistoryBackend) HistoryEntryByID(_ context.Context, owner, logicalDialogID, entryID string) (agentserviceclient.HistoryEntryLookup, error) {
	f.owner, f.logical, f.cursor = owner, logicalDialogID, entryID
	return f.entry, f.err
}

func (f *fakeHistoryBackend) ReadHistoryReplica(_ context.Context, owner, logical string, limit int, cursor string) (historyreplica.ReadPage, error) {
	f.owner, f.logical, f.limit, f.cursor = owner, logical, limit, cursor
	return f.read, f.err
}

func (f *fakeHistoryBackend) HistoryReceiptByOrigin(_ context.Context, owner, nodeID, commandID string) (historyreplica.ReceiptLookup, error) {
	f.owner, f.logical, f.cursor = owner, nodeID, commandID
	return f.receipt, f.err
}

func (f *fakeHistoryBackend) HistoryTextManifest(context.Context, string, string, string) (historyreplica.TextManifest, error) {
	return historyreplica.TextManifest{}, errors.New("not used")
}

func (f *fakeHistoryBackend) HistoryTextChunk(context.Context, string, string, string, int64) (historyreplica.TextChunk, error) {
	return historyreplica.TextChunk{}, errors.New("not used")
}

func TestHistoryReadsUseSessionOwnerAndRemainReadOnly(t *testing.T) {
	server := setup(t, upstream)
	backend := &fakeHistoryBackend{read: historyreplica.ReadPage{
		SchemaID: historyreplica.ReadSchemaID, LogicalDialogID: historyLogicalID,
		Entries: []historyreplica.TranscriptEntry{}, Facts: []historyreplica.ExecutionFact{}, Receipts: []historyreplica.ReceiptRevision{},
		StreamCoverage: []historyreplica.StreamCoverage{{
			StreamID: historyNodeID, ThroughSeq: 0, ChainHash: historyreplica.ChainGenesis(historyNodeID),
			Complete: true, ObservedAt: "2026-09-17T00:00:00Z",
		}},
	}}
	server.history = backend
	cookie, csrf := login(t, server)

	if response := request(server, http.MethodGet, "", "/api/v2/history/dialogs/"+historyLogicalID+"?limit=7&cursor=next", "", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous read status=%d body=%s", response.Code, response.Body.String())
	}
	response := request(server, http.MethodGet, "", "/api/v2/history/dialogs/"+historyLogicalID+"?limit=7&cursor=next", cookie, "")
	if response.Code != http.StatusOK || backend.owner != "1-1" || backend.logical != historyLogicalID || backend.limit != 7 || backend.cursor != "next" {
		t.Fatalf("read status=%d owner=%q logical=%q limit=%d cursor=%q body=%s", response.Code, backend.owner, backend.logical, backend.limit, backend.cursor, response.Body.String())
	}
	if response := request(server, http.MethodPost, `{}`, "/api/v2/history/dialogs/"+historyLogicalID, cookie, csrf); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("history mutation status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request(server, http.MethodGet, "", "/api/v2/history/dialogs/not-a-uuid", cookie, ""); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid dialog status=%d body=%s", response.Code, response.Body.String())
	}

	backend.receipt = historyreplica.ReceiptLookup{SchemaID: historyreplica.ReceiptSchemaID}
	response = request(server, http.MethodGet, "", "/api/v2/history/receipts/"+historyNodeID+"/"+historyCommandID, cookie, "")
	if response.Code != http.StatusOK || backend.owner != "1-1" || backend.logical != historyNodeID || backend.cursor != historyCommandID {
		t.Fatalf("receipt status=%d owner=%q node=%q command=%q body=%s", response.Code, backend.owner, backend.logical, backend.cursor, response.Body.String())
	}
}

func TestHistorySearchAndEntryUseSessionOwner(t *testing.T) {
	server := setup(t, upstream)
	backend := &fakeHistoryBackend{
		search: historysearch.Page{SchemaID: historysearch.SchemaID, Query: historysearch.Query{Q: "needle"}, Items: []historysearch.Item{}, ObservedAt: "2026-09-18T08:00:00Z"},
		entry:  agentserviceclient.HistoryEntryLookup{SchemaID: historysearch.EntryLookupSchemaID, ObservedAt: "2026-09-18T08:00:00Z"},
	}
	server.history = backend
	cookie, _ := login(t, server)

	response := request(server, http.MethodGet, "", "/api/v2/history/search?q=needle&limit=50&nodeId="+historyNodeID+"&archived=true&cursor=next", cookie, "")
	if response.Code != http.StatusOK || backend.owner != "1-1" || backend.limit != 50 || backend.cursor != "next" ||
		backend.searchQuery.Q != "needle" || backend.searchQuery.NodeID != historyNodeID || backend.searchQuery.Archived == nil || !*backend.searchQuery.Archived {
		t.Fatalf("search status=%d backend=%+v body=%s", response.Code, backend, response.Body.String())
	}

	response = request(server, http.MethodGet, "", "/api/v2/history/dialogs/"+historyLogicalID+"/entries/"+historyCommandID, cookie, "")
	if response.Code != http.StatusOK || backend.owner != "1-1" || backend.logical != historyLogicalID || backend.cursor != historyCommandID {
		t.Fatalf("entry status=%d owner=%q logical=%q entry=%q body=%s", response.Code, backend.owner, backend.logical, backend.cursor, response.Body.String())
	}

	for _, path := range []string{
		"/api/v2/history/search?limit=50",
		"/api/v2/history/search?q=needle&archived=maybe",
		"/api/v2/history/search?q=needle&limit=051",
		"/api/v2/history/dialogs/not-a-uuid/entries/" + historyCommandID,
	} {
		if got := request(server, http.MethodGet, "", path, cookie, ""); got.Code != http.StatusBadRequest {
			t.Fatalf("invalid path=%s status=%d body=%s", path, got.Code, got.Body.String())
		}
	}

	backend.err = &agentserviceclient.Fault{Status: http.StatusGone, Code: "search_snapshot_expired"}
	if got := request(server, http.MethodGet, "", "/api/v2/history/search?q=needle&cursor=expired", cookie, ""); got.Code != http.StatusGone || !strings.Contains(got.Body.String(), "search_snapshot_expired") {
		t.Fatalf("expired status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestHistorySearchDoesNotConsumeControlCapacity(t *testing.T) {
	identity := harnessFixture(t, "read.identity")
	receipt := harnessFixture(t, "receipt.6.queue.resume")
	resume := harnessFixture(t, "command.6.queue.resume")
	server := setupHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write(receipt)
			return
		}
		_, _ = w.Write(identity)
	})
	backend := &blockingHistoryBackend{
		fakeHistoryBackend: fakeHistoryBackend{search: historysearch.Page{
			SchemaID: historysearch.SchemaID, Query: historysearch.Query{Q: "needle"},
			Items: []historysearch.Item{}, ObservedAt: "2026-09-18T08:00:00Z",
		}},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	released := false
	defer func() {
		if !released {
			close(backend.release)
		}
	}()
	server.history = backend
	cookie, csrf := login(t, server)
	searchDone := make(chan int, 1)
	go func() {
		searchDone <- request(server, http.MethodGet, "", "/api/v2/history/search?q=needle", cookie, "").Code
	}()
	select {
	case <-backend.entered:
	case <-time.After(time.Second):
		t.Fatal("history search did not enter the backend")
	}
	if generalInFlight, controlInFlight := len(server.general), len(server.control); generalInFlight != 1 || controlInFlight != 0 {
		t.Fatalf("blocked search capacity general=%d control=%d", generalInFlight, controlInFlight)
	}
	response := request(server, http.MethodPost, string(resume), harnessPath+"/commands", cookie, csrf)
	if response.Code != http.StatusAccepted {
		t.Fatalf("control was starved by history search: status=%d body=%s", response.Code, response.Body.String())
	}
	t.Logf("history_search_capacity general_in_flight=1 control_in_flight_before=0 control_status=%d", response.Code)
	close(backend.release)
	released = true
	select {
	case status := <-searchDone:
		if status != http.StatusOK {
			t.Fatalf("history search status=%d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("history search did not finish after release")
	}
}
