package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/store"
)

type fakeStore struct {
	owner         string
	after         string
	limit         int
	result        store.ListResult
	bindingOwner  string
	bindingNode   string
	bindingAfter  string
	bindingLimit  int
	bindingResult store.BindingListResult
	err           error
}

func (f *fakeStore) CheckSchema(context.Context) error { return f.err }
func (f *fakeStore) ListInventory(_ context.Context, owner, after string, limit int) (store.ListResult, error) {
	f.owner, f.after, f.limit = owner, after, limit
	return f.result, f.err
}
func (f *fakeStore) ListDialogBindings(_ context.Context, owner, node, after string, limit int) (store.BindingListResult, error) {
	f.bindingOwner, f.bindingNode, f.bindingAfter, f.bindingLimit = owner, node, after, limit
	return f.bindingResult, f.err
}

func TestInventoryUsesOnlyTrustedOwnerHeaderAndOwnerBoundCursor(t *testing.T) {
	last := "10000000-0000-4000-8000-000000000001"
	database := &fakeStore{result: store.ListResult{
		Items: []model.InventoryItem{{
			NodeID: last, Name: "Agent", Engine: "cursor", SourceMode: "fixture",
			Host:             model.Host{HostID: "20000000-0000-4000-8000-000000000001", Name: "Mac"},
			RegistrationMode: "legacy_readonly", Status: "readonly",
			State:   model.StateAxes{Process: "unknown", Connection: "unknown", Readiness: "unknown", Occupancy: "unknown"},
			Actions: model.ActionsFor("readonly"), DialogCount: 20_000,
		}},
		HasMore: true, After: last,
	}}
	server, err := New(database)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/inventory?limit=1", nil)
	request.Header.Set(OwnerHeader, "owner-1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || database.owner != "owner-1" || database.limit != 1 {
		t.Fatalf("status=%d owner=%q limit=%d body=%s", response.Code, database.owner, database.limit, response.Body.String())
	}
	var page model.InventoryPage
	if json.Unmarshal(response.Body.Bytes(), &page) != nil || page.NextCursor == nil ||
		len(page.Items) != 1 || page.Items[0].DialogCount != 20_000 ||
		strings.Contains(response.Body.String(), `"dialogs"`) || response.Body.Len() > 4096 {
		t.Fatalf("missing next cursor: %s", response.Body.String())
	}

	foreign := httptest.NewRequest(http.MethodGet, "/internal/v1/inventory?cursor="+*page.NextCursor, nil)
	foreign.Header.Set(OwnerHeader, "owner-2")
	foreignResponse := httptest.NewRecorder()
	server.ServeHTTP(foreignResponse, foreign)
	if foreignResponse.Code != http.StatusBadRequest || database.owner != "owner-1" {
		t.Fatalf("foreign cursor reached store: status=%d owner=%q", foreignResponse.Code, database.owner)
	}
}

func TestDialogBindingsAreBoundedAndCursorIsOwnerAndNodeScoped(t *testing.T) {
	nodeID := "10000000-0000-4000-8000-000000000001"
	nodeDialogID := "20000000-0000-4000-8000-000000000001"
	database := &fakeStore{bindingResult: store.BindingListResult{
		Items: []model.DialogMapping{{
			NodeDialogID: nodeDialogID, LogicalDialogID: "30000000-0000-4000-8000-000000000001", BindingVersion: 1,
		}},
		HasMore: true, After: nodeDialogID,
	}}
	server, err := New(database)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/dialog-bindings?nodeId="+nodeID+"&limit=1", nil)
	request.Header.Set(OwnerHeader, "owner-1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || database.bindingOwner != "owner-1" || database.bindingNode != nodeID || database.bindingLimit != 1 {
		t.Fatalf("status=%d owner=%q node=%q limit=%d body=%s", response.Code, database.bindingOwner, database.bindingNode, database.bindingLimit, response.Body.String())
	}
	var page model.DialogBindingPage
	if json.Unmarshal(response.Body.Bytes(), &page) != nil || page.NextCursor == nil || page.SchemaID != model.BindingsSchemaID {
		t.Fatalf("invalid bindings page: %s", response.Body.String())
	}

	otherNode := "10000000-0000-4000-8000-000000000002"
	foreign := httptest.NewRequest(http.MethodGet, "/internal/v1/dialog-bindings?nodeId="+otherNode+"&cursor="+*page.NextCursor, nil)
	foreign.Header.Set(OwnerHeader, "owner-1")
	foreignResponse := httptest.NewRecorder()
	server.ServeHTTP(foreignResponse, foreign)
	if foreignResponse.Code != http.StatusBadRequest || database.bindingNode != nodeID {
		t.Fatalf("foreign node cursor reached store: status=%d node=%q", foreignResponse.Code, database.bindingNode)
	}
}

func TestInventoryFailureIsSafeAndRetryable(t *testing.T) {
	database := &fakeStore{err: errors.New("raw database details")}
	server, _ := New(database)
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/inventory", nil)
	request.Header.Set(OwnerHeader, "owner-1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable ||
		response.Body.String() == "" ||
		contains(response.Body.String(), "raw database") {
		t.Fatalf("unsafe failure: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPrivateAPIRejectsOversizedCursorAndHealthQuery(t *testing.T) {
	database := &fakeStore{}
	server, _ := New(database)
	for _, target := range []string{
		"/internal/v1/inventory?cursor=" + strings.Repeat("a", 513),
		"/internal/v1/inventory?cursor=",
		"/internal/v1/inventory?limit=",
		"/internal/v1/dialog-bindings?nodeId=10000000-0000-4000-8000-000000000001&cursor=",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set(OwnerHeader, "owner-1")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || database.owner != "" || database.bindingOwner != "" {
			t.Fatalf("invalid query reached store: target=%s status=%d owner=%q bindingOwner=%q", target, response.Code, database.owner, database.bindingOwner)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/internal/v1/healthz?probe=foreign", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("health query status=%d body=%s", response.Code, response.Body.String())
	}
}

func contains(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
