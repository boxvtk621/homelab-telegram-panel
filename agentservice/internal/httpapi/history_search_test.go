package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/store"
)

type searchFakeStore struct {
	*historyFakeStore
	createdSpec store.HistorySearchSpec
	createdHash string
	position    store.HistorySearchPosition
	result      store.HistorySearchResult
	err         error
}

func (f *searchFakeStore) UpsertHistoryDialogMetadata(_ context.Context, _ string, logical string, value model.HistoryDialogMetadata) (model.HistoryDialogMetadata, error) {
	value.LogicalDialogID = logical
	return value, f.err
}
func (f *searchFakeStore) CreateHistorySearch(_ context.Context, _ string, spec store.HistorySearchSpec, hash string, _ int) (store.HistorySearchResult, error) {
	f.createdSpec, f.createdHash = spec, hash
	return f.result, f.err
}
func (f *searchFakeStore) ReadHistorySearch(_ context.Context, _ string, _ model.HistorySearchQuery, position store.HistorySearchPosition, _ int) (store.HistorySearchResult, error) {
	f.position = position
	return f.result, f.err
}
func (f *searchFakeStore) HistoryEntryByID(context.Context, string, string, string) (model.HistoryEntryLookup, error) {
	return model.HistoryEntryLookup{}, f.err
}

func TestHistorySearchCursorIsSignedOwnerAndQueryBound(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	position := store.HistorySearchPosition{
		SnapshotID: "10000000-0000-4000-8000-000000000001",
		Offset:     50, QueryHash: strings.Repeat("a", 64), ExpiresAt: "2099-01-01T00:00:00Z",
	}
	cursor := encodeSearchCursor(key, "owner-a", position)
	decoded, err := decodeSearchCursor(key, cursor, "owner-a")
	if err != nil || decoded != position {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	if _, err := decodeSearchCursor(key, cursor, "owner-b"); err == nil {
		t.Fatal("cursor crossed owner scope")
	}
	mutated := cursor[:len(cursor)-1] + "A"
	if _, err := decodeSearchCursor(key, mutated, "owner-a"); err == nil {
		t.Fatal("mutated cursor passed signature")
	}
}

func TestHistorySearchRouteCreatesSnapshotAndReturnsSignedCursor(t *testing.T) {
	database := &searchFakeStore{historyFakeStore: &historyFakeStore{fakeStore: &fakeStore{}}}
	database.result = store.HistorySearchResult{HasMore: true, Position: store.HistorySearchPosition{
		SnapshotID: "10000000-0000-4000-8000-000000000001", Offset: 20,
		ExpiresAt: "2099-01-01T00:00:00Z",
	}, Page: model.HistorySearchPage{SchemaID: model.HistorySearchSchemaID, Items: []model.HistorySearchItem{}}}
	server, _ := NewWithWorkerToken(database, testWorkerToken)
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/history-search?q=%D0%BE%D1%88%D0%B8%D0%B1%D0%BA%D0%B0+uuid&limit=20", nil)
	request.Header.Set(OwnerHeader, "owner-a")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || database.createdSpec.Words != "ошибка uuid" || database.createdHash == "" || !strings.Contains(response.Body.String(), `"nextCursor":"`) {
		t.Fatalf("status=%d spec=%+v hash=%q body=%s", response.Code, database.createdSpec, database.createdHash, response.Body.String())
	}
}

func TestHistorySearchRouteMapsExpiredSnapshotToGone(t *testing.T) {
	database := &searchFakeStore{historyFakeStore: &historyFakeStore{fakeStore: &fakeStore{}}, err: store.ErrHistorySearchExpired}
	server, _ := NewWithWorkerToken(database, testWorkerToken)
	spec, _ := store.ParseHistorySearchQuery(model.HistorySearchQuery{Q: "needle"})
	position := store.HistorySearchPosition{SnapshotID: "10000000-0000-4000-8000-000000000001", Offset: 1,
		QueryHash: store.HistorySearchQueryHash(spec), ExpiresAt: "2099-01-01T00:00:00Z"}
	cursor := encodeSearchCursor(server.historyCursorKey, "owner-a", position)
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/history-search?q=needle&cursor="+cursor, nil)
	request.Header.Set(OwnerHeader, "owner-a")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusGone || !strings.Contains(response.Body.String(), "search_snapshot_expired") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
