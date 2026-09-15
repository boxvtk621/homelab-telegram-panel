package panel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/agentserviceclient"
)

type inventoryFixture struct {
	owner, nodeID, cursor string
	limit                 int
	page                  agentserviceclient.Page
	dialogPage            agentserviceclient.DialogPage
	err                   error
	closed                bool
}

func (f *inventoryFixture) DialogBindings(_ context.Context, owner, nodeID string, limit int, cursor string) (agentserviceclient.DialogPage, error) {
	f.owner, f.nodeID, f.limit, f.cursor = owner, nodeID, limit, cursor
	return f.dialogPage, f.err
}

func (f *inventoryFixture) Inventory(_ context.Context, owner string, limit int, cursor string) (agentserviceclient.Page, error) {
	f.owner, f.limit, f.cursor = owner, limit, cursor
	return f.page, f.err
}

func TestDialogBindingsBFFUsesAuthenticatedOwnerAndExactNode(t *testing.T) {
	nodeID := "20000000-0000-4000-8000-000000000001"
	server := setup(t, upstream)
	fixture := &inventoryFixture{dialogPage: agentserviceclient.DialogPage{
		SchemaID: agentserviceclient.BindingsSchema, NodeID: nodeID, Items: []agentserviceclient.Dialog{},
	}}
	server.inventory = fixture
	cookie, _ := login(t, server)
	got := request(server, http.MethodGet, "", "/api/v2/agents/"+nodeID+"/dialogs?limit=100&cursor=next", cookie, "")
	if got.Code != http.StatusOK || fixture.owner != "1-1" || fixture.nodeID != nodeID || fixture.limit != 100 || fixture.cursor != "next" {
		t.Fatalf("status=%d owner=%q node=%q limit=%d cursor=%q body=%s", got.Code, fixture.owner, fixture.nodeID, fixture.limit, fixture.cursor, got.Body.String())
	}

	fixture.owner = ""
	invalid := request(server, http.MethodGet, "", "/api/v2/agents/not-a-uuid/dialogs", cookie, "")
	if invalid.Code != http.StatusBadRequest || fixture.owner != "" {
		t.Fatalf("invalid node reached service: status=%d owner=%q", invalid.Code, fixture.owner)
	}
}

func (f *inventoryFixture) Close() { f.closed = true }

func TestInventoryBFFUsesAuthenticatedOwnerAndNoBrowserCredentials(t *testing.T) {
	server := setup(t, upstream)
	fixture := &inventoryFixture{page: agentserviceclient.Page{SchemaID: agentserviceclient.InventorySchema, Items: []agentserviceclient.Item{}}}
	server.inventory = fixture
	cookie, _ := login(t, server)

	r := httptest.NewRequest(http.MethodGet, origin+"/api/v2/agents?limit=100&cursor=next", nil)
	r.AddCookie(&http.Cookie{Name: server.sessionCookieName(), Value: cookie})
	r.Header.Set(agentserviceclient.OwnerHeader, "foreign-owner")
	r.Header.Set("Authorization", "Bearer browser-secret")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != http.StatusOK || fixture.owner != "1-1" || fixture.limit != 100 || fixture.cursor != "next" {
		t.Fatalf("status=%d owner=%q limit=%d cursor=%q body=%s", w.Code, fixture.owner, fixture.limit, fixture.cursor, w.Body.String())
	}
	if got := request(server, http.MethodGet, "", "/api/v2/session", cookie, ""); got.Code != http.StatusOK ||
		!strings.Contains(got.Body.String(), `"inventory_enabled":true`) {
		t.Fatalf("inventory capability missing: status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestInventoryPollingDoesNotRenewIdleSession(t *testing.T) {
	nodeID := "20000000-0000-4000-8000-000000000001"
	for _, requestPath := range []string{
		"/api/v2/agents",
		"/api/v2/agents/" + nodeID + "/dialogs",
	} {
		t.Run(requestPath, func(t *testing.T) {
			server := setup(t, upstream)
			server.inventory = &inventoryFixture{
				page: agentserviceclient.Page{SchemaID: agentserviceclient.InventorySchema, Items: []agentserviceclient.Item{}},
				dialogPage: agentserviceclient.DialogPage{
					SchemaID: agentserviceclient.BindingsSchema, NodeID: nodeID, Items: []agentserviceclient.Dialog{},
				},
			}
			now := time.Now()
			server.sessions.now = func() time.Time { return now }
			cookie, _ := login(t, server)
			for minute := 1; minute <= 29; minute++ {
				now = now.Add(time.Minute)
				if got := request(server, http.MethodGet, "", requestPath, cookie, ""); got.Code != http.StatusOK {
					t.Fatalf("minute %d: status=%d body=%s", minute, got.Code, got.Body.String())
				}
			}
			now = now.Add(time.Minute)
			if got := request(server, http.MethodGet, "", requestPath, cookie, ""); got.Code != http.StatusUnauthorized {
				t.Fatalf("polling extended idle TTL: status=%d body=%s", got.Code, got.Body.String())
			}
		})
	}
}

func TestInventoryBFFRejectsInvalidQueriesBeforePrivateService(t *testing.T) {
	server := setup(t, upstream)
	fixture := &inventoryFixture{}
	server.inventory = fixture
	cookie, _ := login(t, server)
	for _, path := range []string{
		"/api/v2/agents?owner=foreign",
		"/api/v2/agents?limit=0",
		"/api/v2/agents?limit=01",
		"/api/v2/agents?limit=",
		"/api/v2/agents?cursor=",
		"/api/v2/agents?cursor=a&cursor=b",
	} {
		if got := request(server, http.MethodGet, "", path, cookie, ""); got.Code != http.StatusBadRequest {
			t.Fatalf("%s accepted: %d %s", path, got.Code, got.Body.String())
		}
	}
	if fixture.owner != "" {
		t.Fatal("invalid browser query reached agent-service")
	}
}

func TestInventoryBFFReturnsOnlySafePrivateFailure(t *testing.T) {
	server := setup(t, upstream)
	server.inventory = &inventoryFixture{err: &agentserviceclient.Fault{
		Status: http.StatusServiceUnavailable, Code: "inventory_unavailable", Retryable: true,
	}}
	cookie, _ := login(t, server)
	got := request(server, http.MethodGet, "", "/api/v2/agents", cookie, "")
	if got.Code != http.StatusServiceUnavailable || got.Body.String() != "{\"error\":\"inventory_unavailable\"}\n" {
		t.Fatalf("unsafe failure: status=%d body=%s", got.Code, got.Body.String())
	}

	server.inventory = &inventoryFixture{err: errors.New("postgres password raw-secret")}
	got = request(server, http.MethodGet, "", "/api/v2/agents", cookie, "")
	if got.Code != http.StatusServiceUnavailable || strings.Contains(got.Body.String(), "raw-secret") {
		t.Fatalf("internal detail escaped: status=%d body=%s", got.Code, got.Body.String())
	}
}
