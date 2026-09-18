package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/configdraft"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/hostadapterclient"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/registry"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/store"
)

type fakeStore struct {
	owner               string
	after               string
	limit               int
	result              store.ListResult
	bindingOwner        string
	bindingNode         string
	bindingAfter        string
	bindingLimit        int
	bindingResult       store.BindingListResult
	hostOwner           string
	hostID              string
	hostAfter           string
	hostLimit           int
	hostInput           model.HostUpsert
	hostObservation     model.HostObservation
	probeRevision       int64
	hostResult          model.HostRecord
	hostListResult      store.HostListResult
	operationOwner      string
	operation           model.OperationIntent
	acceptResult        store.AcceptResult
	statusOwner         string
	statusID            string
	statusResult        model.OperationStatus
	targetResult        model.OperationTargetStatus
	claimedResult       store.ClaimedOperation
	claim               store.OperationClaim
	update              store.OperationUpdate
	registryRaw         []byte
	registryVersion     int64
	registryHash        string
	registryIntent      model.RegistryOperationIntent
	registryStatus      model.RegistryOperationStatus
	registryCandidate   registry.Verified
	registryAffected    []string
	registryNewNode     string
	registryEnvelopeErr error
	registryStatusErr   error
	registryReserveErr  error
	hostObservationErr  error
	err                 error
	configurationDraft  model.ConfigurationDraft
	configurationSaves  int
	logicalOwner        string
	logicalOperationID  string
	logicalRequest      model.LogicalDeleteRequest
	logicalAdvance      model.LogicalDeleteAdvance
	logicalStatus       model.LogicalDeleteStatus
	logicalErr          error
}

type historyFakeStore struct {
	*fakeStore
	page          model.HistoryExportPage
	importResult  model.HistoryImportResult
	readOwner     string
	readLogical   string
	readPosition  store.HistoryReadPosition
	readLimit     int
	readResult    store.HistoryReadResult
	receiptOwner  string
	receiptNode   string
	receiptID     string
	receiptResult model.HistoryReceiptLookup
	historyErr    error
}

func (f *historyFakeStore) ApplyHistoryReplicaPage(_ context.Context, owner string, page model.HistoryExportPage) (model.HistoryImportResult, error) {
	f.owner, f.page = owner, page
	return f.importResult, f.historyErr
}

func (f *historyFakeStore) ReadHistoryReplica(_ context.Context, owner, logicalDialogID string, position store.HistoryReadPosition, limit int) (store.HistoryReadResult, error) {
	f.readOwner, f.readLogical, f.readPosition, f.readLimit = owner, logicalDialogID, position, limit
	return f.readResult, f.historyErr
}

func (f *historyFakeStore) HistoryReceiptByOrigin(_ context.Context, owner, nodeID, commandID string) (model.HistoryReceiptLookup, error) {
	f.receiptOwner, f.receiptNode, f.receiptID = owner, nodeID, commandID
	return f.receiptResult, f.historyErr
}

func (f *historyFakeStore) HistoryTextManifest(context.Context, string, string, string) (model.HistoryTextManifest, error) {
	return model.HistoryTextManifest{}, f.historyErr
}

func (f *historyFakeStore) HistoryTextChunk(context.Context, string, string, string, int64) (model.HistoryTextChunk, error) {
	return model.HistoryTextChunk{}, f.historyErr
}

type fakeHostAdapter struct {
	owner       string
	secret      model.HostSecretInput
	host        model.HostRecord
	observation model.HostObservation
	err         error
}

func (f *fakeHostAdapter) Provision(_ context.Context, owner string, input model.HostSecretInput) (model.HostSecretProvision, error) {
	f.owner = owner
	f.secret = input
	f.secret.PrivateKey = append([]byte(nil), input.PrivateKey...)
	f.secret.Passphrase = append([]byte(nil), input.Passphrase...)
	f.secret.Payload = append([]byte(nil), input.Payload...)
	return model.HostSecretProvision{
		SchemaID: model.HostSecretProvisionSchemaID, OperationID: input.OperationID, Kind: input.Kind,
		Status: "provisioned", CredentialRef: "cred_fixture", Created: true,
	}, f.err
}

func (f *fakeHostAdapter) ProvisionStatus(_ context.Context, owner, operationID string) (model.HostSecretProvision, error) {
	f.owner = owner
	return model.HostSecretProvision{
		SchemaID: model.HostSecretProvisionSchemaID, OperationID: operationID, Kind: "ssh",
		Status: "provisioned", CredentialRef: "cred_fixture",
	}, f.err
}

func (f *fakeHostAdapter) Probe(_ context.Context, owner string, host model.HostRecord) (model.HostObservation, error) {
	f.owner, f.host = owner, host
	return f.observation, f.err
}

const testWorkerToken = "test-worker-token-0000000000000001"

func (f *fakeStore) CheckSchema(context.Context) error { return f.err }
func (f *fakeStore) GetConfigurationDraft(_ context.Context, owner, nodeID string) (model.ConfigurationDraft, error) {
	f.owner, f.statusID = owner, nodeID
	return f.configurationDraft, f.err
}
func (f *fakeStore) SaveConfigurationDraft(_ context.Context, owner, nodeID string, input model.ConfigurationSave, validation configdraft.Result) (model.ConfigurationDraft, error) {
	f.configurationSaves++
	f.owner, f.statusID = owner, nodeID
	f.configurationDraft.RawJSONText, f.configurationDraft.RawDockerfileText, f.configurationDraft.Validation = input.RawJSONText, input.RawDockerfileText, validation.Validation
	return f.configurationDraft, f.err
}
func (f *fakeStore) ListInventory(_ context.Context, owner, after string, limit int) (store.ListResult, error) {
	f.owner, f.after, f.limit = owner, after, limit
	return f.result, f.err
}
func (f *fakeStore) ListDialogBindings(_ context.Context, owner, node, after string, limit int) (store.BindingListResult, error) {
	f.bindingOwner, f.bindingNode, f.bindingAfter, f.bindingLimit = owner, node, after, limit
	return f.bindingResult, f.err
}
func (f *fakeStore) UpsertHost(_ context.Context, owner string, input model.HostUpsert) (model.HostRecord, error) {
	f.hostOwner, f.hostID, f.hostInput = owner, input.HostID, input
	return f.hostResult, f.err
}
func (f *fakeStore) GetHost(_ context.Context, owner, hostID string) (model.HostRecord, error) {
	f.hostOwner, f.hostID = owner, hostID
	return f.hostResult, f.err
}
func (f *fakeStore) ListHosts(_ context.Context, owner, after string, limit int) (store.HostListResult, error) {
	f.hostOwner, f.hostAfter, f.hostLimit = owner, after, limit
	return f.hostListResult, f.err
}
func (f *fakeStore) BeginHostProbe(_ context.Context, owner, hostID string, expectedHostVersion int64) (model.HostRecord, int64, error) {
	f.hostOwner, f.hostID = owner, hostID
	if f.probeRevision == 0 {
		f.probeRevision = 1
	}
	if f.hostResult.HostVersion != expectedHostVersion {
		return model.HostRecord{}, 0, store.ErrHostConflict
	}
	return f.hostResult, f.probeRevision, f.err
}
func (f *fakeStore) RecordHostObservation(_ context.Context, owner string, observation model.HostObservation, probeRevision int64) (model.HostRecord, error) {
	f.hostOwner, f.hostID, f.hostObservation = owner, observation.HostID, observation
	f.probeRevision = probeRevision
	return f.hostResult, f.hostObservationErr
}
func (f *fakeStore) AcceptOperation(_ context.Context, owner string, operation model.OperationIntent) (store.AcceptResult, error) {
	f.operationOwner, f.operation = owner, operation
	return f.acceptResult, f.err
}
func (f *fakeStore) GetOperation(_ context.Context, owner, operationID string) (model.OperationStatus, error) {
	f.statusOwner, f.statusID = owner, operationID
	return f.statusResult, f.err
}
func (f *fakeStore) GetOperationTarget(_ context.Context, owner, nodeID string) (model.OperationTargetStatus, error) {
	f.operationOwner, f.statusID = owner, nodeID
	return f.targetResult, f.err
}
func (f *fakeStore) ClaimNextOperation(_ context.Context, owner, nodeID, workerID string, _ time.Duration) (store.ClaimedOperation, error) {
	f.operationOwner, f.statusID = owner, nodeID
	f.claim.WorkerID = workerID
	return f.claimedResult, f.err
}
func (f *fakeStore) CheckOperationAuthority(_ context.Context, owner, nodeID string, claim store.OperationClaim) error {
	f.operationOwner, f.statusID, f.claim = owner, nodeID, claim
	return f.err
}
func (f *fakeStore) MarkOperationSent(_ context.Context, owner string, claim store.OperationClaim) (store.OperationClaim, error) {
	f.operationOwner, f.claim = owner, claim
	return claim, f.err
}
func (f *fakeStore) AdvanceOperation(_ context.Context, owner string, claim store.OperationClaim, update store.OperationUpdate) (store.OperationClaim, error) {
	f.operationOwner, f.claim, f.update = owner, claim, update
	return claim, f.err
}
func (f *fakeStore) GetRegistryEnvelope(context.Context, string) ([]byte, int64, string, error) {
	return append([]byte(nil), f.registryRaw...), f.registryVersion, f.registryHash, f.registryEnvelopeErr
}
func (f *fakeStore) ReserveRegistryOperation(_ context.Context, _ string, intent model.RegistryOperationIntent, candidate registry.Verified, affected []string, newNode string) (store.RegistryReserveResult, error) {
	f.registryIntent, f.registryCandidate = intent, candidate
	f.registryAffected, f.registryNewNode = append([]string(nil), affected...), newNode
	return store.RegistryReserveResult{Status: f.registryStatus}, f.registryReserveErr
}
func (f *fakeStore) GetRegistryOperation(context.Context, string, string) (model.RegistryOperationStatus, error) {
	return f.registryStatus, f.registryStatusErr
}
func (f *fakeStore) GetRegistryOperationIntent(context.Context, string, string) (model.RegistryOperationIntent, error) {
	return f.registryIntent, f.registryStatusErr
}
func (f *fakeStore) MarkRegistryOperationSent(_ context.Context, _ string, _ model.RegistryOperationCommand) (model.RegistryOperationStatus, error) {
	return f.registryStatus, f.err
}
func (f *fakeStore) MarkRegistryOperationUnknown(_ context.Context, _ string, _ model.RegistryOperationCommand) (model.RegistryOperationStatus, error) {
	return f.registryStatus, f.err
}
func (f *fakeStore) FailRegistryOperation(_ context.Context, _ string, _ model.RegistryOperationFailure) (model.RegistryOperationStatus, error) {
	return f.registryStatus, f.err
}
func (f *fakeStore) FinishRegistryOperation(_ context.Context, _ string, _ model.RegistryOperationFinish, candidate registry.Verified) (model.RegistryOperationStatus, error) {
	f.registryCandidate = candidate
	return f.registryStatus, f.err
}
func (f *fakeStore) BeginLogicalDelete(_ context.Context, owner string, request model.LogicalDeleteRequest) (model.LogicalDeleteStatus, error) {
	f.logicalOwner, f.logicalOperationID, f.logicalRequest = owner, request.OperationID, request
	return f.logicalStatus, f.logicalErr
}
func (f *fakeStore) GetLogicalDelete(_ context.Context, owner, operationID string) (model.LogicalDeleteStatus, error) {
	f.logicalOwner, f.logicalOperationID = owner, operationID
	return f.logicalStatus, f.logicalErr
}
func (f *fakeStore) AdvanceLogicalDelete(_ context.Context, owner string, request model.LogicalDeleteAdvance) (model.LogicalDeleteStatus, error) {
	f.logicalOwner, f.logicalOperationID, f.logicalAdvance = owner, request.OperationID, request
	return f.logicalStatus, f.logicalErr
}

func logicalDeleteStatusFixture() model.LogicalDeleteStatus {
	return model.LogicalDeleteStatus{
		SchemaID: model.LogicalDeleteSchemaID, OperationID: "81000000-0000-4000-8000-000000000001",
		RequestHash: strings.Repeat("a", 64), CommandID: "81000000-0000-4000-8000-000000000002",
		LogicalDialogID: "81000000-0000-4000-8000-000000000003", ExpectedBindingVersion: 1,
		ExpectedDialogVersion: 2, NodeID: "81000000-0000-4000-8000-000000000004",
		NodeDialogID: "81000000-0000-4000-8000-000000000005", RegistryVersion: 1, IdentityEpoch: 1,
		Phase: "accepted", EffectState: "not_sent", OperationVersion: 1, UpdatedAt: "2026-09-17T10:00:00Z",
	}
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

func TestConfigurationValidateAndSaveAreInertAndPreserveInvalidRaw(t *testing.T) {
	nodeID := "20000000-0000-4000-8000-000000000001"
	database := &fakeStore{configurationDraft: model.ConfigurationDraft{SchemaID: model.ConfigurationDraftSchema, NodeID: nodeID, DraftVersion: 1}}
	adapter := &fakeHostAdapter{}
	server, _ := New(database)
	if err := server.SetHostAdapter(adapter); err != nil {
		t.Fatal(err)
	}
	raw := `{"schemaVersion":2,"name":"invalid but durable","engine":"cursor","deployment":{"kind":"external","endpointRef":"fixture"},"profile":{"mode":"universal","basePrompt":"","instructions":[],"mcp":[],"access":{"default":"deny","nativeTools":[]}},"unknown":true}`
	validationBody, _ := json.Marshal(model.ConfigurationInput{SchemaID: model.ConfigurationValidateSchema, RawJSONText: raw, RawDockerfileText: "FROM scratch\n"})
	validate := httptest.NewRequest(http.MethodPost, "/internal/v1/configuration-drafts/validate", strings.NewReader(string(validationBody)))
	validate.Header.Set(OwnerHeader, "owner-1")
	validate.Header.Set("Content-Type", "application/json")
	validated := httptest.NewRecorder()
	server.ServeHTTP(validated, validate)
	if validated.Code != http.StatusOK || !strings.Contains(validated.Body.String(), `"effectStatus":"none"`) || !strings.Contains(validated.Body.String(), `"unknown_field"`) {
		t.Fatalf("validation status=%d body=%s", validated.Code, validated.Body.String())
	}
	saveBody, _ := json.Marshal(model.ConfigurationSave{SchemaID: model.ConfigurationSaveSchema, ExpectedDraftVersion: 0, RawJSONText: raw, RawDockerfileText: "FROM scratch\n"})
	save := httptest.NewRequest(http.MethodPost, "/internal/v1/configuration-drafts/"+nodeID, strings.NewReader(string(saveBody)))
	save.Header.Set(OwnerHeader, "owner-1")
	save.Header.Set("Content-Type", "application/json")
	saved := httptest.NewRecorder()
	server.ServeHTTP(saved, save)
	if saved.Code != http.StatusOK || database.configurationDraft.RawJSONText != raw || database.configurationDraft.Validation.Valid || database.configurationSaves != 1 {
		t.Fatalf("save status=%d draft=%+v", saved.Code, database.configurationDraft)
	}
	if adapter.owner != "" || len(adapter.secret.PrivateKey) != 0 || adapter.host.HostID != "" {
		t.Fatal("validate/save invoked remote host adapter")
	}
}

func TestConfigurationSecretSaveIsRejectedBeforeStoreAndNeverEchoed(t *testing.T) {
	nodeID := "20000000-0000-4000-8000-000000000001"
	canonical := `{"schemaVersion":2,"name":"external","engine":"codex","deployment":{"kind":"external","endpointRef":"harness/codex"},"profile":{"mode":"universal","basePrompt":"","instructions":[],"mcp":[],"access":{"default":"deny","nativeTools":[]}}}`
	for _, fixture := range []struct{ raw, secret string }{
		{`{"\u0074oken":"fixture-secret-value"`, "fixture-secret-value"},
		{`{"profile":{"basePrompt":"-----BEGIN \u0052SA PRIVATE KEY-----"`, "PRIVATE KEY"},
		{strings.Replace(canonical, `"schemaVersion":2`, `"schemaVersion":2,"\u0074oken":"fixture-secret-value"`, 1), "fixture-secret-value"},
		{strings.Replace(canonical, `"basePrompt":""`, `"basePrompt":"-----BEGIN \u0052SA PRIVATE KEY-----"`, 1), "PRIVATE KEY"},
	} {
		database := &fakeStore{configurationDraft: model.ConfigurationDraft{SchemaID: model.ConfigurationDraftSchema, NodeID: nodeID, DraftVersion: 1}}
		server, _ := New(database)
		validateBody, _ := json.Marshal(model.ConfigurationInput{SchemaID: model.ConfigurationValidateSchema, RawJSONText: fixture.raw, RawDockerfileText: "FROM scratch\n"})
		validateRequest := httptest.NewRequest(http.MethodPost, "/internal/v1/configuration-drafts/validate", strings.NewReader(string(validateBody)))
		validateRequest.Header.Set(OwnerHeader, "owner-1")
		validateRequest.Header.Set("Content-Type", "application/json")
		validated := httptest.NewRecorder()
		server.ServeHTTP(validated, validateRequest)
		if validated.Code != http.StatusOK || !strings.Contains(validated.Body.String(), `"valid":false`) || strings.Contains(validated.Body.String(), fixture.secret) {
			t.Fatalf("unsafe validation response: status=%d body=%s", validated.Code, validated.Body.String())
		}

		saveBody, _ := json.Marshal(model.ConfigurationSave{SchemaID: model.ConfigurationSaveSchema, ExpectedDraftVersion: 0, RawJSONText: fixture.raw, RawDockerfileText: "FROM scratch\n"})
		saveRequest := httptest.NewRequest(http.MethodPost, "/internal/v1/configuration-drafts/"+nodeID, strings.NewReader(string(saveBody)))
		saveRequest.Header.Set(OwnerHeader, "owner-1")
		saveRequest.Header.Set("Content-Type", "application/json")
		saved := httptest.NewRecorder()
		server.ServeHTTP(saved, saveRequest)
		if saved.Code != http.StatusBadRequest || database.configurationSaves != 0 {
			t.Fatalf("status=%d saves=%d", saved.Code, database.configurationSaves)
		}
		if strings.Contains(saved.Body.String(), fixture.secret) || database.configurationDraft.RawJSONText != "" {
			t.Fatal("secret was echoed or persisted")
		}
	}
}

func TestConfigurationMalformedRawWithManifestRemainsDurable(t *testing.T) {
	nodeID := "20000000-0000-4000-8000-000000000001"
	database := &fakeStore{configurationDraft: model.ConfigurationDraft{SchemaID: model.ConfigurationDraftSchema, NodeID: nodeID, DraftVersion: 1}}
	server, _ := New(database)
	manifest := &model.BuildContextManifest{Revision: "33333333-3333-4333-8333-333333333333", Assets: []model.BuildContextAsset{{Path: "src/main.go", AssetID: "44444444-4444-4444-8444-444444444444", SHA256: strings.Repeat("a", 64)}}}
	raw := `{"schemaVersion":2`
	body, _ := json.Marshal(model.ConfigurationSave{SchemaID: model.ConfigurationSaveSchema, ExpectedDraftVersion: 0, RawJSONText: raw, RawDockerfileText: "FROM scratch\n", BuildContextManifest: manifest})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/configuration-drafts/"+nodeID, strings.NewReader(string(body)))
	request.Header.Set(OwnerHeader, "owner-1")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || database.configurationSaves != 1 || database.configurationDraft.RawJSONText != raw || database.configurationDraft.Validation.Valid {
		t.Fatalf("status=%d saves=%d draft=%+v", response.Code, database.configurationSaves, database.configurationDraft)
	}
}

func TestConfigurationManifestMismatchIsSavedAsInvalidDraft(t *testing.T) {
	nodeID := "20000000-0000-4000-8000-000000000001"
	database := &fakeStore{configurationDraft: model.ConfigurationDraft{SchemaID: model.ConfigurationDraftSchema, NodeID: nodeID, DraftVersion: 1}}
	server, _ := New(database)
	manifest := &model.BuildContextManifest{Revision: "33333333-3333-4333-8333-333333333333", Assets: []model.BuildContextAsset{}}
	raw := `{"schemaVersion":2,"name":"external","engine":"codex","deployment":{"kind":"external","endpointRef":"harness/codex"},"profile":{"mode":"universal","basePrompt":"","instructions":[],"mcp":[],"access":{"default":"deny","nativeTools":[]}}}`
	body, _ := json.Marshal(model.ConfigurationSave{SchemaID: model.ConfigurationSaveSchema, ExpectedDraftVersion: 0, RawJSONText: raw, RawDockerfileText: "FROM scratch\n", BuildContextManifest: manifest})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/configuration-drafts/"+nodeID, strings.NewReader(string(body)))
	request.Header.Set(OwnerHeader, "owner-1")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || database.configurationSaves != 1 || database.configurationDraft.Validation.Valid {
		t.Fatalf("status=%d saves=%d validation=%+v", response.Code, database.configurationSaves, database.configurationDraft.Validation)
	}
	found := false
	for _, diagnostic := range database.configurationDraft.Validation.Diagnostics {
		found = found || diagnostic.Code == "build_context_binding"
	}
	if !found {
		t.Fatalf("binding diagnostic missing: %+v", database.configurationDraft.Validation.Diagnostics)
	}
}

func TestHostDescriptorsAreOwnerScopedVersionedAndSecretFree(t *testing.T) {
	hostID := "20000000-0000-4000-8000-000000000001"
	record := model.HostRecord{
		SchemaID: model.HostSchemaID, HostID: hostID, HostVersion: 1, DisplayName: "Desktop",
		Transport: "local", TargetRef: "local-desktop", DockerContextRef: "desktop-linux",
		HostPlatform: "darwin", HostArchitecture: "arm64", Availability: "unverified",
		Capabilities: []string{}, RegistryAvailability: "not_configured",
	}
	database := &fakeStore{hostResult: record, hostListResult: store.HostListResult{Items: []model.HostRecord{record}}}
	server, err := New(database)
	if err != nil {
		t.Fatal(err)
	}
	input := model.HostUpsert{
		SchemaID: model.HostUpsertSchemaID, HostID: hostID, ExpectedHostVersion: 0, DisplayName: "Desktop",
		Transport: "local", TargetRef: "local-desktop", DockerContextRef: "desktop-linux",
		HostPlatform: "darwin", HostArchitecture: "arm64",
	}
	raw, _ := json.Marshal(input)
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts", strings.NewReader(string(raw)))
	request.Header.Set(OwnerHeader, "owner-1")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || database.hostOwner != "owner-1" || database.hostInput.HostID != hostID ||
		strings.Contains(response.Body.String(), "privateKey") || strings.Contains(response.Body.String(), "passphrase") {
		t.Fatalf("host upsert failed: status=%d owner=%q body=%s", response.Code, database.hostOwner, response.Body.String())
	}

	list := httptest.NewRequest(http.MethodGet, "/internal/v1/hosts?limit=10", nil)
	list.Header.Set(OwnerHeader, "owner-1")
	listed := httptest.NewRecorder()
	server.ServeHTTP(listed, list)
	if listed.Code != http.StatusOK || database.hostLimit != 10 || !strings.Contains(listed.Body.String(), `"availability":"unverified"`) {
		t.Fatalf("host list failed: status=%d body=%s", listed.Code, listed.Body.String())
	}

	secret := strings.Replace(string(raw[:len(raw)-1]), `"hostArchitecture":"arm64"`, `"hostArchitecture":"arm64","privateKey":"RAW-SECRET"`, 1) + "}"
	bad := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts", strings.NewReader(secret))
	bad.Header.Set(OwnerHeader, "owner-1")
	bad.Header.Set("Content-Type", "application/json")
	badResponse := httptest.NewRecorder()
	server.ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusBadRequest || strings.Contains(badResponse.Body.String(), "RAW-SECRET") {
		t.Fatalf("secret-shaped host input was not safely rejected: %d %s", badResponse.Code, badResponse.Body.String())
	}
}

func TestHostSecretAndProbeFlowStayOwnerScopedAndSecretFree(t *testing.T) {
	hostID := "20000000-0000-4000-8000-000000000001"
	operationID := "30000000-0000-4000-8000-000000000001"
	observedAt := "2026-09-16T10:00:00Z"
	record := model.HostRecord{
		SchemaID: model.HostSchemaID, HostID: hostID, HostVersion: 1, DisplayName: "Desktop",
		Transport: "local", TargetRef: "desktop-local", DockerContextRef: "desktop-linux",
		HostPlatform: "darwin", HostArchitecture: "arm64", ObservedAt: &observedAt,
		Availability: "unavailable", FailureStage: "daemon_ping", FailureCode: "docker_permission_denied",
		NextAction: "Provision daemon access.", Capabilities: []string{}, RegistryAvailability: "not_checked",
	}
	observation := model.HostObservation{
		SchemaID: model.HostObservationSchemaID, HostID: hostID, HostVersion: 1, ObservedAt: observedAt,
		Availability: "unavailable", FailureStage: "daemon_ping", FailureCode: "docker_permission_denied",
		NextAction: "Provision daemon access.", DockerContextRef: "desktop-linux",
		Capabilities: []string{}, RegistryAvailability: "not_checked",
	}
	database := &fakeStore{hostResult: record}
	adapter := &fakeHostAdapter{observation: observation}
	server, err := New(database)
	if err != nil || server.SetHostAdapter(adapter) != nil {
		t.Fatal(err)
	}
	sentinel := []byte("PRIVATE-KEY-SENTINEL")
	secretBody, _ := json.Marshal(model.HostSecretInput{
		SchemaID: model.HostSecretInputSchemaID, OperationID: operationID, Kind: "ssh", PrivateKey: sentinel,
		Passphrase: []byte("passphrase"), Payload: []byte{},
	})
	secretRequest := httptest.NewRequest(http.MethodPost, "/internal/v1/host-secrets", strings.NewReader(string(secretBody)))
	secretRequest.Header.Set(OwnerHeader, "owner-1")
	secretRequest.Header.Set("Content-Type", "application/json")
	secretResponse := httptest.NewRecorder()
	server.ServeHTTP(secretResponse, secretRequest)
	if secretResponse.Code != http.StatusCreated || adapter.owner != "owner-1" || !strings.Contains(string(adapter.secret.PrivateKey), "SENTINEL") ||
		strings.Contains(secretResponse.Body.String(), "SENTINEL") || strings.Contains(secretResponse.Body.String(), "privateKey") ||
		!strings.Contains(secretResponse.Body.String(), `"operationId":"`+operationID+`"`) {
		t.Fatalf("secret status=%d owner=%q body=%s", secretResponse.Code, adapter.owner, secretResponse.Body.String())
	}
	statusRequest := httptest.NewRequest(http.MethodGet, "/internal/v1/host-secrets/"+operationID, nil)
	statusRequest.Header.Set(OwnerHeader, "owner-1")
	statusResponse := httptest.NewRecorder()
	server.ServeHTTP(statusResponse, statusRequest)
	if statusResponse.Code != http.StatusOK || !strings.Contains(statusResponse.Body.String(), `"status":"provisioned"`) ||
		strings.Contains(statusResponse.Body.String(), "privateKey") {
		t.Fatalf("secret status readback=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}

	probeBody, _ := json.Marshal(model.HostProbeRequest{SchemaID: model.HostProbeSchemaID, ExpectedHostVersion: 1})
	probeRequest := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts/"+hostID+"/probe", strings.NewReader(string(probeBody)))
	probeRequest.Header.Set(OwnerHeader, "owner-1")
	probeRequest.Header.Set("Content-Type", "application/json")
	probeResponse := httptest.NewRecorder()
	server.ServeHTTP(probeResponse, probeRequest)
	if probeResponse.Code != http.StatusOK || adapter.host.HostID != hostID || database.hostObservation.FailureCode != "docker_permission_denied" ||
		!strings.Contains(probeResponse.Body.String(), `"failureCode":"docker_permission_denied"`) {
		t.Fatalf("probe status=%d host=%q observation=%+v body=%s", probeResponse.Code, adapter.host.HostID, database.hostObservation, probeResponse.Body.String())
	}
}

func TestHostSecretProvisionPreservesConflictAndOwnerScopedNotFound(t *testing.T) {
	operationID := "30000000-0000-4000-8000-000000000002"
	adapter := &fakeHostAdapter{err: hostadapterclient.ErrSecretConflict}
	server, _ := New(&fakeStore{})
	_ = server.SetHostAdapter(adapter)
	body, _ := json.Marshal(model.HostSecretInput{
		SchemaID: model.HostSecretInputSchemaID, OperationID: operationID, Kind: "registry",
		PrivateKey: []byte{}, Passphrase: []byte{}, Payload: []byte("registry"),
	})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/host-secrets", strings.NewReader(string(body)))
	request.Header.Set(OwnerHeader, "owner-1")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "secret_operation_conflict") || strings.Contains(response.Body.String(), "registry") {
		t.Fatalf("conflict status=%d body=%s", response.Code, response.Body.String())
	}

	adapter.err = hostadapterclient.ErrSecretNotFound
	statusRequest := httptest.NewRequest(http.MethodGet, "/internal/v1/host-secrets/"+operationID, nil)
	statusRequest.Header.Set(OwnerHeader, "owner-2")
	statusResponse := httptest.NewRecorder()
	server.ServeHTTP(statusResponse, statusRequest)
	if statusResponse.Code != http.StatusNotFound || !strings.Contains(statusResponse.Body.String(), "secret_provision_not_found") {
		t.Fatalf("not found status=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}
}

func TestFailedAdapterProbeInvalidatesStoredReadyObservation(t *testing.T) {
	hostID := "20000000-0000-4000-8000-000000000001"
	host := model.HostRecord{
		SchemaID: model.HostSchemaID, HostID: hostID, HostVersion: 2, DisplayName: "Desktop",
		Transport: "local", TargetRef: "desktop-local", DockerContextRef: "desktop-linux",
		HostPlatform: "darwin", HostArchitecture: "arm64", Availability: "ready",
		Capabilities: []string{"docker-api-read"}, RegistryAvailability: "not_configured",
	}
	database := &fakeStore{hostResult: host}
	adapter := &fakeHostAdapter{err: errors.New("private socket unavailable")}
	server, _ := New(database)
	_ = server.SetHostAdapter(adapter)
	body, _ := json.Marshal(model.HostProbeRequest{SchemaID: model.HostProbeSchemaID, ExpectedHostVersion: 2})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts/"+hostID+"/probe", strings.NewReader(string(body)))
	request.Header.Set(OwnerHeader, "owner-1")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || database.hostObservation.Availability != "unavailable" ||
		database.hostObservation.FailureCode != "host_probe_unavailable" || database.hostObservation.IdentitySHA256 != "" {
		t.Fatalf("status=%d observation=%+v body=%s", response.Code, database.hostObservation, response.Body.String())
	}
}

func TestSupersededHostProbeCannotRestoreReady(t *testing.T) {
	hostID := "20000000-0000-4000-8000-000000000001"
	host := model.HostRecord{
		SchemaID: model.HostSchemaID, HostID: hostID, HostVersion: 2, DisplayName: "Desktop",
		Transport: "local", TargetRef: "desktop-local", DockerContextRef: "desktop-linux",
		HostPlatform: "darwin", HostArchitecture: "arm64", Availability: "unverified",
		Capabilities: []string{}, RegistryAvailability: "not_configured",
	}
	observation := model.HostObservation{
		SchemaID: model.HostObservationSchemaID, HostID: hostID, HostVersion: 2,
		ObservedAt: "2026-09-16T12:00:00Z", Availability: "ready", DaemonID: "daemon-old",
		DockerContextRef: "desktop-linux", ContextEndpoint: "sha256:" + strings.Repeat("a", 64),
		EngineOS: "linux", Architecture: "arm64", APIVersion: "1.56", EngineVersion: "29.8.0",
		Capabilities: []string{"docker-api-read"}, IdentitySHA256: strings.Repeat("b", 64), RegistryAvailability: "not_configured",
	}
	database := &fakeStore{hostResult: host, probeRevision: 7, hostObservationErr: store.ErrHostConflict}
	server, _ := New(database)
	_ = server.SetHostAdapter(&fakeHostAdapter{observation: observation})
	body, _ := json.Marshal(model.HostProbeRequest{SchemaID: model.HostProbeSchemaID, ExpectedHostVersion: 2})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts/"+hostID+"/probe", strings.NewReader(string(body)))
	request.Header.Set(OwnerHeader, "owner-1")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"probe_superseded"`) || database.probeRevision != 7 {
		t.Fatalf("status=%d revision=%d body=%s", response.Code, database.probeRevision, response.Body.String())
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

func TestOperationAcceptanceIsDurableBefore202AndStatusIsReadOnly(t *testing.T) {
	intent := model.OperationIntent{
		SchemaID: model.OperationSchemaID, OperationID: "op-1", Kind: "adapter.fixture",
		Target: model.OperationTarget{
			NodeID: "10000000-0000-4000-8000-000000000001", HostID: "20000000-0000-4000-8000-000000000001",
			RegistrationRevision: 1, RegistrationEpoch: 1, Generation: 0,
		},
		Step: model.OperationStepIntent{StepID: "apply-1", Action: "adapter.fixture.apply", ResourceIDs: []string{"node:agent-1"}},
	}
	receipt := model.OperationReceipt{
		SchemaID: model.OperationReceiptSchemaID, OperationID: intent.OperationID,
		RequestHash: strings.Repeat("a", 64), Target: intent.Target, AcceptedGeneration: 1,
		AcceptedAt: "2026-09-14T10:00:00Z",
	}
	database := &fakeStore{acceptResult: store.AcceptResult{Receipt: receipt}}
	server, _ := New(database)
	body, _ := json.Marshal(intent)
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/operations", strings.NewReader(string(body)))
	request.Header.Set(OwnerHeader, "owner-1")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || database.operationOwner != "owner-1" || database.operation.OperationID != "op-1" {
		t.Fatalf("status=%d owner=%q operation=%+v body=%s", response.Code, database.operationOwner, database.operation, response.Body.String())
	}
	var returned model.OperationReceipt
	if json.Unmarshal(response.Body.Bytes(), &returned) != nil || returned != receipt {
		t.Fatalf("receipt changed: %+v", returned)
	}

	status := model.OperationStatus{
		SchemaID: model.OperationStatusSchemaID, Receipt: receipt, Phase: "accepted", EffectState: "not_sent",
		OperationVersion: 1, UpdatedAt: receipt.AcceptedAt,
	}
	database.statusResult = status
	read := httptest.NewRequest(http.MethodGet, "/internal/v1/operations/op-1", nil)
	read.Header.Set(OwnerHeader, "owner-1")
	readResponse := httptest.NewRecorder()
	server.ServeHTTP(readResponse, read)
	if readResponse.Code != http.StatusOK || database.statusOwner != "owner-1" || database.statusID != "op-1" {
		t.Fatalf("status read failed: %d %+v", readResponse.Code, database)
	}
}

func TestOperationAPIRejectsAmbiguousInputsAndMapsConflicts(t *testing.T) {
	server, _ := New(&fakeStore{err: store.ErrOperationConflict})
	valid := `{"schemaId":"agent-operation-v1","operationId":"op-1","kind":"adapter.fixture","target":{"nodeId":"10000000-0000-4000-8000-000000000001","hostId":"20000000-0000-4000-8000-000000000001","registrationRevision":1,"registrationEpoch":1,"generation":0},"step":{"stepId":"apply-1","action":"adapter.fixture.apply","resourceIds":["container:agent-1"]}}`
	for _, body := range []string{
		strings.Replace(valid, `"operationId":"op-1"`, `"operationId":"op-1","operationId":"op-1"`, 1),
		strings.Replace(valid, `"operationId"`, `"OperationId"`, 1),
		strings.Replace(valid, `"container:agent-1"`, `"bad/resource"`, 1),
	} {
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/operations", strings.NewReader(body))
		request.Header.Set(OwnerHeader, "owner-1")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("ambiguous input status=%d body=%s", response.Code, response.Body.String())
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/operations", strings.NewReader(valid))
	request.Header.Set(OwnerHeader, "owner-1")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "raw") {
		t.Fatalf("conflict status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRegistryOperationReservesSignedDeltaAndReplaysTerminalReceiptBeforeCAS(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256([]byte("registry-http-node"))
	node := model.RegistryNode{
		NodeID: "10000000-0000-4000-8000-000000000001", Name: "Agent", Adapter: "cursor",
		URL: "https://agent.invalid", CertificateSHA256: hex.EncodeToString(pin[:]),
	}
	currentManifest := model.RegistryManifest{RegistryVersion: 1, OwnerID: "owner-1", Mode: "fixture", Nodes: []model.RegistryNode{node}}
	currentRaw := signedHTTPRegistry(t, private, currentManifest)
	current, err := registry.Verify(currentRaw, publicKeyPEM(t, public))
	if err != nil {
		t.Fatal(err)
	}
	candidateManifest := currentManifest
	candidateManifest.SchemaID = "harness-router-registry-v1"
	candidateManifest.RegistryVersion = 2
	candidateManifest.WireSchemaSHA256 = "5bd97f2ea08854a8e56d46ff11a1539e6bc54e8ca6d42841b366561accba73d9"
	candidateManifest.Nodes = append([]model.RegistryNode(nil), currentManifest.Nodes...)
	candidateManifest.Nodes[0].RegistrationRevision = 1
	candidateManifest.Nodes[0].RegistrationEpoch = 1
	candidateManifest.Nodes[0].Compatibility = "compatible"
	candidateRaw := signedHTTPRegistry(t, private, candidateManifest)
	candidate, err := registry.Verify(candidateRaw, publicKeyPEM(t, public))
	if err != nil {
		t.Fatal(err)
	}
	intent := model.RegistryOperationIntent{
		SchemaID: model.RegistryOperationSchemaID, OperationID: "registry-upgrade-1",
		Expected: model.RegistryExpected{RegistryVersion: 1, RegistrySHA256: current.ManifestSHA256},
		Registry: candidateRaw, NewNodeHostID: nil,
	}
	requestHash, err := model.RegistryOperationRequestHash(intent)
	if err != nil {
		t.Fatal(err)
	}
	status := model.RegistryOperationStatus{
		SchemaID: model.RegistryOperationStatusSchemaID,
		Receipt: model.RegistryOperationReceipt{
			SchemaID: model.RegistryOperationReceiptSchemaID, OperationID: intent.OperationID,
			RequestHash: requestHash, ExpectedRegistryVersion: 1, ExpectedRegistrySHA256: current.ManifestSHA256,
			CandidateRegistryVersion: 2, CandidateRegistrySHA256: candidate.ManifestSHA256,
			AffectedNodeIDs: []string{node.NodeID}, AcceptedAt: "2026-09-14T10:00:00Z",
		},
		Phase: "accepted", EffectState: "not_sent", OperationVersion: 1, UpdatedAt: "2026-09-14T10:00:00Z",
	}
	database := &fakeStore{
		registryRaw: currentRaw, registryVersion: 1, registryHash: current.ManifestSHA256,
		registryStatus: status, registryStatusErr: store.ErrOperationNotFound,
	}
	server, err := NewWithCapabilities(database, testWorkerToken, publicKeyPEM(t, public))
	if err != nil {
		t.Fatal(err)
	}
	response := postOperationJSON(t, server, "/internal/v1/registry-operations", intent)
	if response.Code != http.StatusAccepted || database.registryIntent.OperationID != intent.OperationID ||
		database.registryCandidate.ManifestSHA256 != candidate.ManifestSHA256 ||
		len(database.registryAffected) != 1 || database.registryAffected[0] != node.NodeID {
		t.Fatalf("reserve status=%d body=%s database=%+v", response.Code, response.Body.String(), database)
	}

	unauthorizedBody, _ := json.Marshal(intent)
	unauthorized := httptest.NewRequest(http.MethodPost, "/internal/v1/registry-operations", strings.NewReader(string(unauthorizedBody)))
	unauthorized.Header.Set(OwnerHeader, "owner-1")
	unauthorized.Header.Set("Content-Type", "application/json")
	unauthorizedResponse := httptest.NewRecorder()
	server.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusForbidden {
		t.Fatalf("missing worker token status=%d body=%s", unauthorizedResponse.Code, unauthorizedResponse.Body.String())
	}

	database.registryStatusErr = nil
	database.registryStatus.Phase = "succeeded"
	database.registryStatus.EffectState = "acknowledged"
	database.registryStatus.ResultCode = ptr("registry:" + candidate.ManifestSHA256)
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rotatedSignerServer, err := NewWithCapabilities(database, testWorkerToken, publicKeyPEM(t, otherPublic))
	if err != nil {
		t.Fatal(err)
	}
	repeat := postOperationJSON(t, rotatedSignerServer, "/internal/v1/registry-operations", intent)
	if repeat.Code != http.StatusAccepted || !strings.Contains(repeat.Body.String(), `"phase":"succeeded"`) {
		t.Fatalf("terminal replay status=%d body=%s", repeat.Code, repeat.Body.String())
	}
	mutated := intent
	mutated.NewNodeHostID = ptr("20000000-0000-4000-8000-000000000001")
	conflict := postOperationJSON(t, rotatedSignerServer, "/internal/v1/registry-operations", mutated)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("same id different payload status=%d body=%s", conflict.Code, conflict.Body.String())
	}
}

func TestExternalEnrollmentSignsExactCandidateAndReplaysOneDurableOperation(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer := publicKeyPEM(t, public)
	privatePEM := privateKeyPEM(t, private)
	pin := sha256.Sum256([]byte("existing-node"))
	existing := model.RegistryNode{
		NodeID: "20000000-0000-4000-8000-000000000001", Name: "Existing", Adapter: "cursor",
		URL: "https://10.20.30.10:9443", CertificateSHA256: hex.EncodeToString(pin[:]),
		RegistrationRevision: 1, RegistrationEpoch: 1, Compatibility: "compatible",
	}
	currentManifest := model.RegistryManifest{
		SchemaID: registry.RouterSchemaID, RegistryVersion: 2, OwnerID: "owner-1", Mode: "fixture",
		WireSchemaSHA256: registry.WireSchemaSHA256, Nodes: []model.RegistryNode{existing},
	}
	currentRaw := signedHTTPRegistry(t, private, currentManifest)
	current, err := registry.Verify(currentRaw, signer)
	if err != nil {
		t.Fatal(err)
	}
	request := model.ExternalEnrollmentRequest{
		SchemaID: model.ExternalEnrollmentSchemaID, OperationID: "r10-enroll-one",
		HostID: "10000000-0000-4000-8000-000000000001", ExpectedHostVersion: 3,
		NodeID: "20000000-0000-4000-8000-000000000002", Name: "External Codex", Adapter: "codex",
		EndpointURI: "https://10.20.30.40:9443", CertificateSHA256: strings.Repeat("b", 64),
	}
	host := model.HostRecord{
		SchemaID: model.HostSchemaID, HostID: request.HostID, HostVersion: 3, DisplayName: "External host",
		Transport: "local", TargetRef: "local-external", DockerContextRef: "must-not-cross",
		HostPlatform: "darwin", HostArchitecture: "arm64", Availability: "ready", Capabilities: []string{}, RegistryAvailability: "ready",
	}
	binding, bindingHash, err := externalEnrollmentBinding(request, host)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := registry.Sign(externalEnrollmentManifest(current.Manifest, request, bindingHash), privatePEM, signer)
	if err != nil {
		t.Fatal(err)
	}
	hostID := request.HostID
	intent := model.RegistryOperationIntent{
		SchemaID: model.RegistryOperationSchemaID, OperationID: request.OperationID,
		Expected: model.RegistryExpected{RegistryVersion: current.Manifest.RegistryVersion, RegistrySHA256: current.ManifestSHA256},
		Registry: candidate.Envelope, NewNodeHostID: &hostID, ExternalBinding: &binding,
	}
	requestHash, err := model.RegistryOperationRequestHash(intent)
	if err != nil {
		t.Fatal(err)
	}
	status := model.RegistryOperationStatus{
		SchemaID: model.RegistryOperationStatusSchemaID,
		Receipt: model.RegistryOperationReceipt{
			SchemaID: model.RegistryOperationReceiptSchemaID, OperationID: request.OperationID, RequestHash: requestHash,
			ExpectedRegistryVersion: 2, ExpectedRegistrySHA256: current.ManifestSHA256,
			CandidateRegistryVersion: 3, CandidateRegistrySHA256: candidate.ManifestSHA256,
			AffectedNodeIDs: []string{request.NodeID}, AcceptedAt: "2026-09-17T10:00:00Z",
		},
		Phase: "accepted", EffectState: "not_sent", OperationVersion: 1, UpdatedAt: "2026-09-17T10:00:00Z",
	}
	database := &fakeStore{
		hostResult: host, registryRaw: currentRaw, registryVersion: 2, registryHash: current.ManifestSHA256,
		registryStatus: status, registryStatusErr: store.ErrOperationNotFound,
	}
	server, err := NewWithCapabilities(database, testWorkerToken, signer)
	if err != nil || server.SetRegistrySigningKey(privatePEM) != nil {
		t.Fatal("cannot configure enrollment signer", err)
	}
	response := postOperationJSON(t, server, "/internal/v1/external-enrollments", request)
	if response.Code != http.StatusAccepted || database.registryNewNode != request.NodeID || len(database.registryAffected) != 1 ||
		database.registryCandidate.ManifestSHA256 != candidate.ManifestSHA256 {
		t.Fatalf("status=%d body=%s database=%+v", response.Code, response.Body.String(), database)
	}
	var plan model.ExternalEnrollmentPlan
	if json.Unmarshal(response.Body.Bytes(), &plan) != nil || plan.Binding == nil || *plan.Binding != binding ||
		plan.Status.Receipt.RequestHash != requestHash || strings.Contains(response.Body.String(), "must-not-cross") {
		t.Fatalf("unsafe or invalid plan: %s", response.Body.String())
	}

	database.registryStatusErr = nil
	database.hostResult.HostVersion++
	database.hostResult.TargetRef = "mutated-after-reservation"
	repeat := postOperationJSON(t, server, "/internal/v1/external-enrollments", request)
	if repeat.Code != http.StatusAccepted || repeat.Body.String() != response.Body.String() {
		t.Fatalf("same operation did not replay exact plan: status=%d body=%s", repeat.Code, repeat.Body.String())
	}
	mutated := request
	mutated.CertificateSHA256 = strings.Repeat("c", 64)
	conflict := postOperationJSON(t, server, "/internal/v1/external-enrollments", mutated)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("same operation accepted different identity: status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	staleHost := request
	staleHost.OperationID = "r10-stale-host"
	database.registryStatusErr = store.ErrOperationNotFound
	stale := postOperationJSON(t, server, "/internal/v1/external-enrollments", staleHost)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), `"code":"host_version_conflict"`) {
		t.Fatalf("stale host status=%d body=%s", stale.Code, stale.Body.String())
	}
	database.hostResult = host
	publicEndpoint := request
	publicEndpoint.OperationID = "r10-public-endpoint"
	publicEndpoint.EndpointURI = "https://8.8.8.8:9443"
	database.registryStatusErr = store.ErrOperationNotFound
	rejected := postOperationJSON(t, server, "/internal/v1/external-enrollments", publicEndpoint)
	if rejected.Code != http.StatusUnprocessableEntity {
		t.Fatalf("public endpoint status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	legacyManifest := model.RegistryManifest{
		RegistryVersion: 2, OwnerID: "owner-1", Mode: "fixture",
		Nodes: []model.RegistryNode{{
			NodeID: existing.NodeID, Name: existing.Name, Adapter: existing.Adapter,
			URL: existing.URL, CertificateSHA256: existing.CertificateSHA256,
		}},
	}
	legacyRaw := signedHTTPRegistry(t, private, legacyManifest)
	legacy, err := registry.Verify(legacyRaw, signer)
	if err != nil {
		t.Fatal(err)
	}
	database.registryRaw, database.registryVersion, database.registryHash = legacyRaw, 2, legacy.ManifestSHA256
	legacyRequest := request
	legacyRequest.OperationID = "r10-legacy-registry"
	legacyRejected := postOperationJSON(t, server, "/internal/v1/external-enrollments", legacyRequest)
	if legacyRejected.Code != http.StatusConflict || !strings.Contains(legacyRejected.Body.String(), `"code":"registry_projection_required"`) {
		t.Fatalf("legacy registry status=%d body=%s", legacyRejected.Code, legacyRejected.Body.String())
	}
}

func TestOperationWorkerAPIExposesExactTargetClaimAuthorityAndTransitions(t *testing.T) {
	nodeID := "10000000-0000-4000-8000-000000000001"
	hostID := "20000000-0000-4000-8000-000000000001"
	target := model.OperationTargetStatus{
		SchemaID:     model.OperationTargetSchemaID,
		Target:       model.OperationTarget{NodeID: nodeID, HostID: hostID, RegistrationRevision: 3, RegistrationEpoch: 5, Generation: 7},
		RegistryMode: "fixture", RegistrationMode: "compatible",
	}
	intent := model.OperationIntent{
		SchemaID: model.OperationSchemaID, OperationID: "op-worker", Kind: "adapter.fixture", Target: target.Target,
		Step: model.OperationStepIntent{StepID: "apply-1", Action: "adapter.fixture.apply", ResourceIDs: []string{"container:agent-1"}},
	}
	requestHash, err := model.OperationRequestHash(intent)
	if err != nil {
		t.Fatal(err)
	}
	claim := store.OperationClaim{
		OperationID: intent.OperationID, RequestHash: requestHash, NodeID: nodeID, Generation: 8, WorkerID: "worker-1",
		Token: "30000000-0000-4000-8000-000000000001", Version: 2,
		ExpiresAt: time.Date(2026, 9, 14, 14, 0, 0, 123456000, time.UTC),
	}
	receipt := model.OperationReceipt{
		SchemaID: model.OperationReceiptSchemaID, OperationID: intent.OperationID, RequestHash: requestHash,
		Target: intent.Target, AcceptedGeneration: claim.Generation, AcceptedAt: "2026-09-14T13:59:00Z",
	}
	database := &fakeStore{
		targetResult: target, claimedResult: store.ClaimedOperation{Intent: intent, Claim: claim, EffectState: "not_sent"},
		statusResult: model.OperationStatus{
			SchemaID: model.OperationStatusSchemaID, Receipt: receipt, Phase: "succeeded", EffectState: "acknowledged",
			OperationVersion: 4, UpdatedAt: "2026-09-14T14:00:00Z",
		},
	}
	server, _ := NewWithWorkerToken(database, testWorkerToken)

	read := httptest.NewRequest(http.MethodGet, "/internal/v1/operation-targets/"+nodeID, nil)
	read.Header.Set(OwnerHeader, "owner-1")
	readResponse := httptest.NewRecorder()
	server.ServeHTTP(readResponse, read)
	if readResponse.Code != http.StatusOK || !strings.Contains(readResponse.Body.String(), `"schemaId":"agent-operation-target-v1"`) {
		t.Fatalf("target status=%d body=%s", readResponse.Code, readResponse.Body.String())
	}

	claimRequest := model.OperationClaimRequest{
		SchemaID: model.OperationClaimSchemaID, NodeID: nodeID, WorkerID: claim.WorkerID, LeaseMilliseconds: 30_000,
	}
	claimResponse := postOperationJSON(t, server, "/internal/v1/operation-workers/claim", claimRequest)
	var work model.OperationWork
	if claimResponse.Code != http.StatusOK || json.Unmarshal(claimResponse.Body.Bytes(), &work) != nil ||
		model.ValidateOperationWork(work) != nil || work.Proof.OperationVersion != claim.Version {
		t.Fatalf("claim status=%d body=%s", claimResponse.Code, claimResponse.Body.String())
	}

	authorityResponse := postOperationJSON(t, server, "/internal/v1/operation-workers/authority", model.OperationAuthorityRequest{
		SchemaID: model.OperationAuthoritySchema, Proof: work.Proof,
	})
	if authorityResponse.Code != http.StatusOK || database.claim.Version != claim.Version {
		t.Fatalf("authority status=%d body=%s", authorityResponse.Code, authorityResponse.Body.String())
	}
	sentResponse := postOperationJSON(t, server, "/internal/v1/operation-workers/sent", model.OperationSentRequest{
		SchemaID: model.OperationSentSchemaID, Proof: work.Proof,
	})
	if sentResponse.Code != http.StatusOK {
		t.Fatalf("sent status=%d body=%s", sentResponse.Code, sentResponse.Body.String())
	}
	advanceResponse := postOperationJSON(t, server, "/internal/v1/operation-workers/advance", model.OperationAdvanceRequest{
		SchemaID: model.OperationAdvanceSchemaID, Proof: work.Proof, Phase: "succeeded", EffectState: "acknowledged",
		ResultCode: ptr("fixture:op-worker"),
	})
	if advanceResponse.Code != http.StatusOK || database.update.Phase != "succeeded" || database.update.EffectState != "acknowledged" {
		t.Fatalf("advance status=%d update=%+v body=%s", advanceResponse.Code, database.update, advanceResponse.Body.String())
	}
}

func TestHistoryReplicaEndpointsEnforceWorkerOwnerAndOfflineReads(t *testing.T) {
	raw, err := os.ReadFile("../../../api/history-replica-v1.fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		ValidPage model.HistoryExportPage `json:"validPage"`
	}
	if json.Unmarshal(raw, &fixtures) != nil || model.ValidateHistoryExportPage(fixtures.ValidPage) != nil {
		t.Fatal("invalid canonical history fixture")
	}
	page := fixtures.ValidPage
	database := &historyFakeStore{fakeStore: &fakeStore{}}
	database.importResult = model.HistoryImportResult{
		SchemaID: model.HistoryExportSchemaID, StreamID: page.StreamID, ImportedThrough: page.Checkpoint.ThroughSeq,
		SourceThrough: page.Checkpoint.ThroughSeq, Complete: true, ObservedAt: "2026-09-17T00:00:02Z",
	}
	database.readResult.Page = model.HistoryReadPage{
		SchemaID: model.HistoryReadSchemaID, LogicalDialogID: page.Identity.LogicalDialogID,
		Entries: []model.HistoryTranscriptEntry{}, Facts: []model.HistoryExecutionFact{}, Receipts: []model.HistoryReceiptRevision{},
		SyncedThrough: page.Checkpoint,
		StreamCoverage: []model.HistoryStreamCoverage{{
			StreamID: page.StreamID, ThroughSeq: page.Checkpoint.ThroughSeq, ChainHash: page.Checkpoint.ChainHash,
			Complete: true, ObservedAt: "2026-09-17T00:00:02Z",
		}},
		ObservedAt: "2026-09-17T00:00:02Z",
	}
	database.readResult.HasMore = true
	database.readResult.Position = store.HistoryReadPosition{
		Horizons: []store.HistoryReadHorizon{{
			StreamID: page.StreamID, ThroughSeq: page.Checkpoint.ThroughSeq, ChainHash: page.Checkpoint.ChainHash,
			Complete: true, ObservedAt: "2026-09-17T00:00:02Z",
		}},
		Checkpoint: page.Checkpoint, ObservedAt: "2026-09-17T00:00:02Z", SourceCapturedAt: page.Checkpoint.CapturedAt,
		Complete: true, EntryCreatedAt: "2026-09-17T00:00:00Z", EntryID: page.Records[0].EntityID,
	}
	server, err := NewWithWorkerToken(database, testWorkerToken)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(page)
	unauthorized := httptest.NewRequest(http.MethodPost, "/internal/v1/history-replica/batches", strings.NewReader(string(body)))
	unauthorized.Header.Set(OwnerHeader, page.Identity.OwnerID)
	unauthorized.Header.Set("Content-Type", "application/json")
	unauthorizedResponse := httptest.NewRecorder()
	server.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusForbidden {
		t.Fatalf("missing worker token status=%d body=%s", unauthorizedResponse.Code, unauthorizedResponse.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, "/internal/v1/history-replica/batches", strings.NewReader(string(body)))
	request.Header.Set(OwnerHeader, page.Identity.OwnerID)
	request.Header.Set(WorkerTokenHeader, testWorkerToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || database.owner != page.Identity.OwnerID || database.page.StreamID != page.StreamID {
		t.Fatalf("import status=%d owner=%q stream=%q body=%s", response.Code, database.owner, database.page.StreamID, response.Body.String())
	}

	read := httptest.NewRequest(http.MethodGet, "/internal/v1/history-replica/dialogs/"+page.Identity.LogicalDialogID+"?limit=7", nil)
	read.Header.Set(OwnerHeader, page.Identity.OwnerID)
	readResponse := httptest.NewRecorder()
	server.ServeHTTP(readResponse, read)
	if readResponse.Code != http.StatusOK || database.readOwner != page.Identity.OwnerID ||
		database.readLogical != page.Identity.LogicalDialogID || database.readLimit != 7 {
		t.Fatalf("read status=%d owner=%q logical=%q limit=%d body=%s", readResponse.Code, database.readOwner, database.readLogical, database.readLimit, readResponse.Body.String())
	}
	var readPage model.HistoryReadPage
	if json.Unmarshal(readResponse.Body.Bytes(), &readPage) != nil || readPage.NextCursor == nil {
		t.Fatalf("paginated read response=%s", readResponse.Body.String())
	}
	continued := httptest.NewRequest(http.MethodGet, "/internal/v1/history-replica/dialogs/"+page.Identity.LogicalDialogID+"?limit=7&cursor="+*readPage.NextCursor, nil)
	continued.Header.Set(OwnerHeader, page.Identity.OwnerID)
	continuedResponse := httptest.NewRecorder()
	server.ServeHTTP(continuedResponse, continued)
	if continuedResponse.Code != http.StatusOK || len(database.readPosition.Horizons) != 1 ||
		database.readPosition.Horizons[0].StreamID != page.StreamID || database.readPosition.EntryID != page.Records[0].EntityID {
		t.Fatalf("history cursor did not round-trip: status=%d position=%+v body=%s", continuedResponse.Code, database.readPosition, continuedResponse.Body.String())
	}

	database.historyErr = store.ErrHistoryReplicaGap
	gap := httptest.NewRequest(http.MethodPost, "/internal/v1/history-replica/batches", strings.NewReader(string(body)))
	gap.Header.Set(OwnerHeader, page.Identity.OwnerID)
	gap.Header.Set(WorkerTokenHeader, testWorkerToken)
	gap.Header.Set("Content-Type", "application/json")
	gapResponse := httptest.NewRecorder()
	server.ServeHTTP(gapResponse, gap)
	if gapResponse.Code != http.StatusConflict || !strings.Contains(gapResponse.Body.String(), `"history_gap"`) {
		t.Fatalf("gap status=%d body=%s", gapResponse.Code, gapResponse.Body.String())
	}
}

func TestLogicalDeleteEndpointsRequireWorkerAndPreserveExactOwnerScope(t *testing.T) {
	status := logicalDeleteStatusFixture()
	database := &fakeStore{logicalStatus: status}
	server, err := NewWithWorkerToken(database, testWorkerToken)
	if err != nil {
		t.Fatal(err)
	}
	request := model.LogicalDeleteRequest{
		SchemaID: model.LogicalDeleteSchemaID, OperationID: status.OperationID, CommandID: status.CommandID,
		LogicalDialogID: status.LogicalDialogID, ExpectedBindingVersion: status.ExpectedBindingVersion,
		ExpectedDialogVersion: status.ExpectedDialogVersion,
	}
	body, _ := json.Marshal(request)
	unauthorized := httptest.NewRequest(http.MethodPost, "/internal/v1/logical-dialog-deletes", strings.NewReader(string(body)))
	unauthorized.Header.Set(OwnerHeader, "owner-1")
	unauthorized.Header.Set("Content-Type", "application/json")
	unauthorizedResponse := httptest.NewRecorder()
	server.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusForbidden || database.logicalOwner != "" {
		t.Fatalf("unauthorized begin status=%d owner=%q body=%s", unauthorizedResponse.Code, database.logicalOwner, unauthorizedResponse.Body.String())
	}

	begin := httptest.NewRequest(http.MethodPost, "/internal/v1/logical-dialog-deletes", strings.NewReader(string(body)))
	begin.Header.Set(OwnerHeader, "owner-1")
	begin.Header.Set(WorkerTokenHeader, testWorkerToken)
	begin.Header.Set("Content-Type", "application/json")
	beginResponse := httptest.NewRecorder()
	server.ServeHTTP(beginResponse, begin)
	if beginResponse.Code != http.StatusOK || database.logicalOwner != "owner-1" || database.logicalRequest != request {
		t.Fatalf("begin status=%d owner=%q request=%+v body=%s", beginResponse.Code, database.logicalOwner, database.logicalRequest, beginResponse.Body.String())
	}

	readWithoutWorker := httptest.NewRequest(http.MethodGet, "/internal/v1/logical-dialog-deletes/"+status.OperationID, nil)
	readWithoutWorker.Header.Set(OwnerHeader, "owner-1")
	readWithoutWorkerResponse := httptest.NewRecorder()
	server.ServeHTTP(readWithoutWorkerResponse, readWithoutWorker)
	if readWithoutWorkerResponse.Code != http.StatusForbidden {
		t.Fatalf("unscoped status=%d body=%s", readWithoutWorkerResponse.Code, readWithoutWorkerResponse.Body.String())
	}
	read := httptest.NewRequest(http.MethodGet, "/internal/v1/logical-dialog-deletes/"+status.OperationID, nil)
	read.Header.Set(OwnerHeader, "owner-1")
	read.Header.Set(WorkerTokenHeader, testWorkerToken)
	readResponse := httptest.NewRecorder()
	server.ServeHTTP(readResponse, read)
	if readResponse.Code != http.StatusOK || database.logicalOperationID != status.OperationID {
		t.Fatalf("status=%d operation=%q body=%s", readResponse.Code, database.logicalOperationID, readResponse.Body.String())
	}

	advance := model.LogicalDeleteAdvance{
		SchemaID: model.LogicalDeleteAdvanceSchemaID, OperationID: status.OperationID, RequestHash: status.RequestHash,
		ExpectedOperationVersion: 1, Action: "hold", HoldVersion: 1, ObservedHoldScopeRevision: 1,
	}
	advanceBody, _ := json.Marshal(advance)
	advanceRequest := httptest.NewRequest(http.MethodPost, "/internal/v1/logical-dialog-deletes/advance", strings.NewReader(string(advanceBody)))
	advanceRequest.Header.Set(OwnerHeader, "owner-1")
	advanceRequest.Header.Set(WorkerTokenHeader, testWorkerToken)
	advanceRequest.Header.Set("Content-Type", "application/json")
	advanceResponse := httptest.NewRecorder()
	server.ServeHTTP(advanceResponse, advanceRequest)
	if advanceResponse.Code != http.StatusOK || database.logicalAdvance != advance {
		t.Fatalf("advance status=%d request=%+v body=%s", advanceResponse.Code, database.logicalAdvance, advanceResponse.Body.String())
	}

	database.logicalErr = store.ErrLogicalDeleteConflict
	conflict := httptest.NewRequest(http.MethodGet, "/internal/v1/logical-dialog-deletes/"+status.OperationID, nil)
	conflict.Header.Set(OwnerHeader, "owner-1")
	conflict.Header.Set(WorkerTokenHeader, testWorkerToken)
	conflictResponse := httptest.NewRecorder()
	server.ServeHTTP(conflictResponse, conflict)
	if conflictResponse.Code != http.StatusConflict || !strings.Contains(conflictResponse.Body.String(), "logical_delete_conflict") {
		t.Fatalf("conflict status=%d body=%s", conflictResponse.Code, conflictResponse.Body.String())
	}
}

func TestHistoryCursorMaximumCoverageFitsPublicBound(t *testing.T) {
	const observedAt = "2026-09-17T00:00:00.123456789Z"
	key := []byte("history-cursor-test-key-32-bytes!")
	hash := strings.Repeat("a", 64)
	position := store.HistoryReadPosition{
		Checkpoint: model.HistoryCheckpoint{
			ThroughSeq: 1, ChainHash: hash, DialogVersion: 1, QueueRevision: 1,
			EntriesHash: hash, FactsHash: hash, ReceiptsHash: hash, TextHash: hash, AssetManifestHash: hash,
			EffectsSummary: model.HistoryEffectsSummary{EntryCount: 1}, Ready: true, CapturedAt: observedAt,
		},
		ObservedAt: observedAt, SourceCapturedAt: observedAt, Complete: true,
		EntryCreatedAt: observedAt, EntryID: model.HistoryStableUUID("cursor-entry", "maximum"),
		FactOccurredAt: observedAt, FactID: model.HistoryStableUUID("cursor-fact", "maximum"),
		ReceiptAcceptedAt: observedAt, ReceiptRevisionID: model.HistoryStableUUID("cursor-receipt", "maximum"),
	}
	for index := range 24 {
		position.Horizons = append(position.Horizons, store.HistoryReadHorizon{
			StreamID: model.HistoryStableUUID("cursor-stream", string(rune(index))), ThroughSeq: model.HistoryMaximumSafeInt,
			ChainHash: hash, Complete: true, ObservedAt: observedAt,
		})
	}
	cursor := encodeHistoryCursor(key, strings.Repeat("o", 200), model.HistoryStableUUID("cursor-dialog", "maximum"), position)
	if len(cursor) > 8192 {
		t.Fatalf("maximum history cursor is %d bytes, want <= 8192", len(cursor))
	}
	decoded, err := decodeHistoryCursor(key, cursor, strings.Repeat("o", 200), model.HistoryStableUUID("cursor-dialog", "maximum"))
	if err != nil || len(decoded.Horizons) != 24 {
		t.Fatalf("maximum history cursor does not round-trip: horizons=%d err=%v", len(decoded.Horizons), err)
	}
	payload, signature, _ := strings.Cut(cursor, ".")
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(raw, []byte(`"m":true`), []byte(`"m":false`), 1)
	if bytes.Equal(tampered, raw) {
		t.Fatal("cursor completeness marker was not found")
	}
	tamperedCursor := base64.RawURLEncoding.EncodeToString(tampered) + "." + signature
	if _, err := decodeHistoryCursor(key, tamperedCursor, strings.Repeat("o", 200), model.HistoryStableUUID("cursor-dialog", "maximum")); err == nil {
		t.Fatal("tampered history cursor was accepted")
	}
}

func postOperationJSON(t *testing.T, server http.Handler, path string, value any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	request.Header.Set(OwnerHeader, "owner-1")
	request.Header.Set(WorkerTokenHeader, testWorkerToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func signedHTTPRegistry(t *testing.T, private ed25519.PrivateKey, manifest model.RegistryManifest) []byte {
	t.Helper()
	canonical, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(struct {
		Manifest  model.RegistryManifest `json:"manifest"`
		Signature string                 `json:"signature"`
	}{Manifest: manifest, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, canonical))})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func publicKeyPEM(t *testing.T, public ed25519.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func privateKeyPEM(t *testing.T, private ed25519.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func ptr(value string) *string { return &value }

func contains(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
