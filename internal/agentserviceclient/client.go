// Package agentserviceclient is the stdlib-only private client used by Panel.
// It cannot reach PostgreSQL, Docker, signing keys, or secret stores; mutation
// methods require an explicit worker token and operate only on durable intents.
package agentserviceclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/boxvtk621/homelab-telegram-panel/internal/historyreplica"
	"github.com/boxvtk621/homelab-telegram-panel/internal/historysearch"
	"github.com/boxvtk621/homelab-telegram-panel/internal/logicaldelete"
	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

const (
	OwnerHeader                  = "X-Agent-Service-Owner"
	InventorySchema              = "agent-management-v1"
	BindingsSchema               = "agent-dialog-bindings-v1"
	HostUpsertSchema             = "agent-host-upsert-v1"
	HostSchema                   = "agent-host-v1"
	HostPageSchema               = "agent-host-page-v1"
	HostSecretSchema             = "docker-secret-input-v1"
	HostSecretProvisionSchema    = "docker-secret-provision-v1"
	HostProbeSchema              = "agent-host-probe-v1"
	ConfigurationDraftSchema     = "agent-configuration-draft-v2"
	ConfigurationValidateSchema  = "agent-configuration-validate-v2"
	ConfigurationSaveSchema      = "agent-configuration-save-v2"
	ExternalEnrollmentSchema     = "external-harness-enrollment-v1"
	ExternalEnrollmentPlanSchema = "external-harness-enrollment-plan-v1"
	RegistryCommandSchema        = "agent-registry-operation-command-v1"
	RegistryFinishSchema         = "agent-registry-operation-finish-v1"
	RegistryFailureSchema        = "agent-registry-operation-failure-v1"
	maximumBody                  = 2 << 20
	maximumConfigurationBody     = 3 << 20
)

var (
	actorPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)
	uuidPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	sha256Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hostKeyPattern = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{20,64}$`)
)

type Action struct {
	Allowed    bool   `json:"allowed"`
	Reason     string `json:"reason,omitempty"`
	NextAction string `json:"nextAction,omitempty"`
}

type Actions struct {
	OpenWorkspace Action `json:"openWorkspace"`
	SendMessage   Action `json:"sendMessage"`
	Lifecycle     Action `json:"lifecycle"`
}

type Host struct {
	HostID string `json:"hostId"`
	Name   string `json:"name"`
}

type State struct {
	Process    string `json:"process"`
	Connection string `json:"connection"`
	Readiness  string `json:"readiness"`
	Occupancy  string `json:"occupancy"`
}

type Metric struct {
	Value      int64  `json:"value"`
	ObservedAt string `json:"observedAt"`
	Source     string `json:"source"`
}

type Dialog struct {
	NodeDialogID    string `json:"nodeDialogId"`
	LogicalDialogID string `json:"logicalDialogId"`
	BindingVersion  int64  `json:"bindingVersion"`
}

type Item struct {
	NodeID           string  `json:"nodeId"`
	Name             string  `json:"name"`
	Engine           string  `json:"engine"`
	SourceMode       string  `json:"sourceMode"`
	Host             Host    `json:"host"`
	RegistrationMode string  `json:"registrationMode"`
	Status           string  `json:"status"`
	State            State   `json:"state"`
	ObservedAt       *string `json:"observedAt"`
	Source           *string `json:"source"`
	PendingCount     *Metric `json:"pendingCount"`
	Actions          Actions `json:"actions"`
	DialogCount      int64   `json:"dialogCount"`
}

type Page struct {
	SchemaID   string  `json:"schemaId"`
	Items      []Item  `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

type DialogPage struct {
	SchemaID   string   `json:"schemaId"`
	NodeID     string   `json:"nodeId"`
	Items      []Dialog `json:"items"`
	NextCursor *string  `json:"nextCursor"`
}

type HistoryImportResult struct {
	SchemaID        string `json:"schemaId"`
	StreamID        string `json:"streamId"`
	ImportedThrough int64  `json:"importedThrough"`
	SourceThrough   int64  `json:"sourceThrough"`
	Duplicate       bool   `json:"duplicate"`
	Complete        bool   `json:"complete"`
	ObservedAt      string `json:"observedAt"`
}

type HostUpsert struct {
	SchemaID               string `json:"schemaId"`
	HostID                 string `json:"hostId"`
	ExpectedHostVersion    int64  `json:"expectedHostVersion"`
	DisplayName            string `json:"displayName"`
	Transport              string `json:"transport"`
	TargetRef              string `json:"targetRef"`
	CredentialRef          string `json:"credentialRef"`
	RegistryCredentialRef  string `json:"registryCredentialRef"`
	ExpectedHostKey        string `json:"expectedHostKey"`
	DockerContextRef       string `json:"dockerContextRef"`
	ExpectedIdentitySHA256 string `json:"expectedIdentitySHA256"`
	HostPlatform           string `json:"hostPlatform"`
	HostArchitecture       string `json:"hostArchitecture"`
}

type DockerHost struct {
	SchemaID               string   `json:"schemaId"`
	HostID                 string   `json:"hostId"`
	HostVersion            int64    `json:"hostVersion"`
	DisplayName            string   `json:"displayName"`
	Transport              string   `json:"transport"`
	TargetRef              string   `json:"targetRef"`
	CredentialRef          string   `json:"credentialRef"`
	RegistryCredentialRef  string   `json:"registryCredentialRef"`
	ExpectedHostKey        string   `json:"expectedHostKey"`
	DockerContextRef       string   `json:"dockerContextRef"`
	ExpectedIdentitySHA256 string   `json:"expectedIdentitySHA256"`
	HostPlatform           string   `json:"hostPlatform"`
	HostArchitecture       string   `json:"hostArchitecture"`
	ObservedAt             *string  `json:"observedAt"`
	Availability           string   `json:"availability"`
	FailureStage           string   `json:"failureStage"`
	FailureCode            string   `json:"failureCode"`
	NextAction             string   `json:"nextAction"`
	HostKeySHA256          string   `json:"hostKeySHA256"`
	DaemonID               string   `json:"daemonId"`
	ContextEndpoint        string   `json:"contextEndpoint"`
	EngineOS               string   `json:"engineOS"`
	Architecture           string   `json:"architecture"`
	APIVersion             string   `json:"apiVersion"`
	EngineVersion          string   `json:"engineVersion"`
	Capabilities           []string `json:"capabilities"`
	IdentitySHA256         string   `json:"identitySHA256"`
	RegistryAvailability   string   `json:"registryAvailability"`
}

type DockerHostPage struct {
	SchemaID   string       `json:"schemaId"`
	Items      []DockerHost `json:"items"`
	NextCursor *string      `json:"nextCursor"`
}

type HostSecretInput struct {
	SchemaID    string `json:"schemaId"`
	OperationID string `json:"operationId"`
	Kind        string `json:"kind"`
	PrivateKey  []byte `json:"privateKey"`
	Passphrase  []byte `json:"passphrase"`
	Payload     []byte `json:"payload"`
}

type HostSecretProvision struct {
	SchemaID      string `json:"schemaId"`
	OperationID   string `json:"operationId"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	CredentialRef string `json:"credentialRef"`
	Created       bool   `json:"-"`
}

type HostProbeRequest struct {
	SchemaID            string `json:"schemaId"`
	ExpectedHostVersion int64  `json:"expectedHostVersion"`
}

type ConfigurationDiagnostic struct {
	Code    string `json:"code"`
	Pointer string `json:"pointer"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
}
type ConfigurationValidationState struct {
	Valid        bool                      `json:"valid"`
	Diagnostics  []ConfigurationDiagnostic `json:"diagnostics"`
	EffectStatus string                    `json:"effectStatus"`
}
type ConfigurationInput struct {
	SchemaID             string                `json:"schemaId"`
	RawJSONText          string                `json:"rawJsonText"`
	RawDockerfileText    string                `json:"rawDockerfileText"`
	BuildContextManifest *BuildContextManifest `json:"buildContextManifest,omitempty"`
}
type ConfigurationSave struct {
	SchemaID             string                `json:"schemaId"`
	ExpectedDraftVersion int64                 `json:"expectedDraftVersion"`
	RawJSONText          string                `json:"rawJsonText"`
	RawDockerfileText    string                `json:"rawDockerfileText"`
	BuildContextManifest *BuildContextManifest `json:"buildContextManifest,omitempty"`
}
type BuildContextAsset struct {
	Path    string `json:"path"`
	AssetID string `json:"assetId"`
	SHA256  string `json:"sha256"`
}
type BuildContextManifest struct {
	Revision string              `json:"revision"`
	Assets   []BuildContextAsset `json:"assets"`
}
type ConfigurationDraft struct {
	SchemaID             string                       `json:"schemaId"`
	NodeID               string                       `json:"nodeId"`
	DraftVersion         int64                        `json:"draftVersion"`
	RawJSONText          string                       `json:"rawJsonText"`
	RawDockerfileText    string                       `json:"rawDockerfileText"`
	BuildContextManifest *BuildContextManifest        `json:"buildContextManifest,omitempty"`
	Validation           ConfigurationValidationState `json:"validation"`
}
type ConfigurationValidation struct {
	SchemaID             string                       `json:"schemaId"`
	BuildContextManifest *BuildContextManifest        `json:"buildContextManifest,omitempty"`
	Validation           ConfigurationValidationState `json:"validation"`
}

type ExternalEnrollmentRequest struct {
	SchemaID            string `json:"schemaId"`
	OperationID         string `json:"operationId"`
	HostID              string `json:"hostId"`
	ExpectedHostVersion int64  `json:"expectedHostVersion"`
	NodeID              string `json:"nodeId"`
	Name                string `json:"name"`
	Adapter             string `json:"adapter"`
	EndpointURI         string `json:"endpointUri"`
	CertificateSHA256   string `json:"certificateSHA256"`
}

type ExternalEndpointBinding struct {
	Kind                       string `json:"kind"`
	NodeID                     string `json:"nodeId"`
	RegistrationRevision       int64  `json:"registrationRevision"`
	RegistrationEpoch          int64  `json:"registrationEpoch"`
	EndpointRevision           int64  `json:"endpointRevision"`
	HostID                     string `json:"hostId"`
	HostVersion                int64  `json:"hostVersion"`
	Transport                  string `json:"transport"`
	TargetRef                  string `json:"targetRef"`
	CredentialRef              string `json:"credentialRef"`
	DockerContextRef           string `json:"dockerContextRef"`
	ExpectedHostKey            string `json:"expectedHostKey"`
	ExpectedHostIdentitySHA256 string `json:"expectedHostIdentitySHA256"`
	HostPlatform               string `json:"hostPlatform"`
	HostArchitecture           string `json:"hostArchitecture"`
	ContainerID                string `json:"containerId"`
	RuntimeGeneration          int64  `json:"runtimeGeneration"`
	Address                    string `json:"address"`
}

type RegistryOperationReceipt struct {
	SchemaID                 string   `json:"schemaId"`
	OperationID              string   `json:"operationId"`
	RequestHash              string   `json:"requestHash"`
	ExpectedRegistryVersion  int64    `json:"expectedRegistryVersion"`
	ExpectedRegistrySHA256   string   `json:"expectedRegistrySHA256"`
	CandidateRegistryVersion int64    `json:"candidateRegistryVersion"`
	CandidateRegistrySHA256  string   `json:"candidateRegistrySHA256"`
	AffectedNodeIDs          []string `json:"affectedNodeIds"`
	AcceptedAt               string   `json:"acceptedAt"`
}

type RegistryOperationStatus struct {
	SchemaID         string                   `json:"schemaId"`
	Receipt          RegistryOperationReceipt `json:"receipt"`
	Phase            string                   `json:"phase"`
	EffectState      string                   `json:"effectState"`
	OperationVersion int64                    `json:"operationVersion"`
	UpdatedAt        string                   `json:"updatedAt"`
	ResultCode       *string                  `json:"resultCode"`
}

type ExternalEnrollmentPlan struct {
	SchemaID    string                   `json:"schemaId"`
	OperationID string                   `json:"operationId"`
	RequestHash string                   `json:"requestHash"`
	NodeID      string                   `json:"nodeId"`
	Registry    json.RawMessage          `json:"registry"`
	Binding     *ExternalEndpointBinding `json:"binding"`
	Status      RegistryOperationStatus  `json:"status"`
}

type RegistryOperationCommand struct {
	SchemaID         string `json:"schemaId"`
	OperationID      string `json:"operationId"`
	RequestHash      string `json:"requestHash"`
	OperationVersion int64  `json:"operationVersion"`
}

type RegistryOperationFinish struct {
	SchemaID         string `json:"schemaId"`
	OperationID      string `json:"operationId"`
	RequestHash      string `json:"requestHash"`
	OperationVersion int64  `json:"operationVersion"`
	RegistryVersion  int64  `json:"registryVersion"`
	RegistrySHA256   string `json:"registrySHA256"`
	EffectState      string `json:"effectState"`
}

type RegistryOperationFailure struct {
	SchemaID         string `json:"schemaId"`
	OperationID      string `json:"operationId"`
	RequestHash      string `json:"requestHash"`
	OperationVersion int64  `json:"operationVersion"`
	ResultCode       string `json:"resultCode"`
}

type Response struct {
	Status int
	Page   Page
}

type Fault struct {
	Status    int
	Code      string
	Retryable bool
}

func (f *Fault) Error() string { return f.Code }

type Client struct {
	http        *http.Client
	probeHTTP   *http.Client
	workerToken string
}

func New(socket string) (*Client, error) {
	return newClient(socket, "")
}

func NewWithWorkerToken(socket, workerToken string) (*Client, error) {
	if !actorPattern.MatchString(workerToken) || len(workerToken) < 32 || len(workerToken) > 128 {
		return nil, errors.New("invalid agent-service worker token")
	}
	return newClient(socket, workerToken)
}

func newClient(socket, workerToken string) (*Client, error) {
	if !strings.HasPrefix(socket, "/") || len(socket) > 100 || strings.TrimSpace(socket) != socket ||
		strings.ContainsAny(socket, "\x00\r\n") {
		return nil, errors.New("invalid agent-service socket")
	}
	dialer := &net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
		ResponseHeaderTimeout: 3 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          4,
		MaxConnsPerHost:       4,
		DisableCompression:    true,
	}
	probeTransport := transport.Clone()
	probeTransport.ResponseHeaderTimeout = 58 * time.Second
	checkRedirect := func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{http: &http.Client{
		Transport:     transport,
		CheckRedirect: checkRedirect,
	}, probeHTTP: &http.Client{Transport: probeTransport, CheckRedirect: checkRedirect}, workerToken: workerToken}, nil
}

func (c *Client) Close() {
	if transport, ok := c.http.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	if c.probeHTTP != nil {
		if transport, ok := c.probeHTTP.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
}

func (c *Client) Inventory(ctx context.Context, owner string, limit int, cursor string) (Page, error) {
	if !actorPattern.MatchString(owner) || limit < 1 || limit > 100 || len(cursor) > 512 {
		return Page{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	body, err := c.read(ctx, owner, "/internal/v1/inventory?"+query.Encode())
	if err != nil {
		return Page{}, err
	}
	var page Page
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&page) != nil || decoder.Decode(new(any)) != io.EOF || !validPage(page) {
		return Page{}, &Fault{Status: http.StatusServiceUnavailable, Code: "invalid_inventory_response", Retryable: true}
	}
	return page, nil
}

func (c *Client) DialogBindings(ctx context.Context, owner, nodeID string, limit int, cursor string) (DialogPage, error) {
	if !actorPattern.MatchString(owner) || !uuidPattern.MatchString(nodeID) || limit < 1 || limit > 100 || len(cursor) > 512 {
		return DialogPage{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	query := url.Values{"nodeId": {nodeID}, "limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	body, err := c.read(ctx, owner, "/internal/v1/dialog-bindings?"+query.Encode())
	if err != nil {
		return DialogPage{}, err
	}
	var page DialogPage
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&page) != nil || decoder.Decode(new(any)) != io.EOF || !validDialogPage(page, nodeID) {
		return DialogPage{}, &Fault{Status: http.StatusServiceUnavailable, Code: "invalid_inventory_response", Retryable: true}
	}
	return page, nil
}

func (c *Client) BeginLogicalDelete(ctx context.Context, owner string, request logicaldelete.Request) (logicaldelete.Status, error) {
	if c.workerToken == "" || !actorPattern.MatchString(owner) || logicaldelete.ValidateRequest(request) != nil {
		return logicaldelete.Status{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, _, err := c.writeWithLimit(ctx, owner, "/internal/v1/logical-dialog-deletes", request, 64<<10, http.StatusOK)
	if err != nil {
		return logicaldelete.Status{}, err
	}
	return decodeLogicalDeleteStatus(body, request.OperationID)
}

func (c *Client) GetLogicalDelete(ctx context.Context, owner, operationID string) (logicaldelete.Status, error) {
	if c.workerToken == "" || !actorPattern.MatchString(owner) || !uuidPattern.MatchString(operationID) {
		return logicaldelete.Status{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, err := c.read(ctx, owner, "/internal/v1/logical-dialog-deletes/"+url.PathEscape(operationID))
	if err != nil {
		return logicaldelete.Status{}, err
	}
	return decodeLogicalDeleteStatus(body, operationID)
}

func (c *Client) AdvanceLogicalDelete(ctx context.Context, owner string, request logicaldelete.AdvanceRequest) (logicaldelete.Status, error) {
	if c.workerToken == "" || !actorPattern.MatchString(owner) || logicaldelete.ValidateAdvance(request) != nil {
		return logicaldelete.Status{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, _, err := c.writeWithLimit(ctx, owner, "/internal/v1/logical-dialog-deletes/advance", request, 2<<20, http.StatusOK)
	if err != nil {
		return logicaldelete.Status{}, err
	}
	return decodeLogicalDeleteStatus(body, request.OperationID)
}

func decodeLogicalDeleteStatus(body []byte, operationID string) (logicaldelete.Status, error) {
	var status logicaldelete.Status
	if !decodeHostResponse(body, &status) || status.OperationID != operationID || logicaldelete.ValidateStatus(status) != nil {
		return logicaldelete.Status{}, &Fault{Status: http.StatusServiceUnavailable, Code: "invalid_logical_delete_response", Retryable: true}
	}
	return status, nil
}

func (c *Client) ApplyHistoryReplica(ctx context.Context, owner string, page historyreplica.ExportPage) (HistoryImportResult, error) {
	if c.workerToken == "" || !actorPattern.MatchString(owner) || page.Identity.OwnerID != owner ||
		historyreplica.ValidatePage(page) != nil {
		return HistoryImportResult{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, _, err := c.writeWithLimit(ctx, owner, "/internal/v1/history-replica/batches", page,
		historyreplica.MaximumPageBytes, http.StatusOK)
	if err != nil {
		return HistoryImportResult{}, err
	}
	var result HistoryImportResult
	if !decodeHostResponse(body, &result) || result.SchemaID != historyreplica.SchemaID ||
		result.StreamID != page.StreamID || result.ImportedThrough < 0 || result.ImportedThrough > result.SourceThrough ||
		result.SourceThrough != page.Checkpoint.ThroughSeq || !uuidPattern.MatchString(result.StreamID) {
		return HistoryImportResult{}, historyContractFault()
	}
	if _, err := time.Parse(time.RFC3339Nano, result.ObservedAt); err != nil {
		return HistoryImportResult{}, historyContractFault()
	}
	return result, nil
}

func (c *Client) ReadHistoryReplica(ctx context.Context, owner, logicalDialogID string, limit int, cursor string) (historyreplica.ReadPage, error) {
	if !actorPattern.MatchString(owner) || !uuidPattern.MatchString(logicalDialogID) ||
		limit < 1 || limit > historyreplica.MaximumPageSize || len(cursor) > 8192 {
		return historyreplica.ReadPage{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	body, err := c.readWithLimit(ctx, owner, "/internal/v1/history-replica/dialogs/"+url.PathEscape(logicalDialogID)+"?"+query.Encode(), historyreplica.MaximumPageBytes)
	if err != nil {
		return historyreplica.ReadPage{}, err
	}
	var page historyreplica.ReadPage
	if !decodeHostResponse(body, &page) || page.SchemaID != historyreplica.ReadSchemaID ||
		page.LogicalDialogID != logicalDialogID || page.Entries == nil || page.Facts == nil || page.Receipts == nil ||
		page.StreamCoverage == nil || len(page.StreamCoverage) == 0 || len(page.StreamCoverage) > 24 ||
		len(page.Entries) > limit || len(page.Facts) > limit || len(page.Receipts) > limit ||
		page.LagMillis < 0 || (page.NextCursor != nil && (*page.NextCursor == "" || len(*page.NextCursor) > 8192)) {
		return historyreplica.ReadPage{}, historyContractFault()
	}
	if _, err := time.Parse(time.RFC3339Nano, page.ObservedAt); err != nil {
		return historyreplica.ReadPage{}, historyContractFault()
	}
	for _, coverage := range page.StreamCoverage {
		if !uuidPattern.MatchString(coverage.StreamID) || coverage.ThroughSeq < 0 ||
			coverage.ThroughSeq > historyreplica.MaximumSafeInt || !sha256Pattern.MatchString(coverage.ChainHash) {
			return historyreplica.ReadPage{}, historyContractFault()
		}
		if _, err := time.Parse(time.RFC3339Nano, coverage.ObservedAt); err != nil {
			return historyreplica.ReadPage{}, historyContractFault()
		}
	}
	return page, nil
}

func (c *Client) SearchHistory(ctx context.Context, owner string, query historysearch.Query, limit int, cursor string) (historysearch.Page, error) {
	if !actorPattern.MatchString(owner) || !validHistorySearchQuery(query) || limit < 1 || limit > historysearch.MaximumPageSize || len(cursor) > 8192 {
		return historysearch.Page{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	values := url.Values{"q": {query.Q}, "limit": {strconv.Itoa(limit)}}
	if query.NodeID != "" {
		values.Set("nodeId", query.NodeID)
	}
	if query.LogicalDialogID != "" {
		values.Set("logicalDialogId", query.LogicalDialogID)
	}
	if query.Archived != nil {
		values.Set("archived", strconv.FormatBool(*query.Archived))
	}
	if cursor != "" {
		values.Set("cursor", cursor)
	}
	body, err := c.readWithLimit(ctx, owner, "/internal/v1/history-search?"+values.Encode(), maximumBody)
	if err != nil {
		return historysearch.Page{}, err
	}
	var page historysearch.Page
	if !decodeHostResponse(body, &page) || !validHistorySearchPage(page, query, limit) {
		return historysearch.Page{}, historySearchContractFault()
	}
	return page, nil
}

type HistoryEntryLookup struct {
	SchemaID   string                         `json:"schemaId"`
	Entry      historyreplica.TranscriptEntry `json:"entry"`
	ObservedAt string                         `json:"observedAt"`
	LagMillis  int64                          `json:"lagMillis"`
	Incomplete bool                           `json:"incomplete"`
}

func (c *Client) HistoryEntryByID(ctx context.Context, owner, logicalDialogID, entryID string) (HistoryEntryLookup, error) {
	if !actorPattern.MatchString(owner) || !uuidPattern.MatchString(logicalDialogID) || !uuidPattern.MatchString(entryID) {
		return HistoryEntryLookup{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, err := c.readWithLimit(ctx, owner, "/internal/v1/history-replica/dialogs/"+url.PathEscape(logicalDialogID)+
		"/entries/"+url.PathEscape(entryID), maximumBody)
	if err != nil {
		return HistoryEntryLookup{}, err
	}
	var result HistoryEntryLookup
	if !decodeHostResponse(body, &result) || result.SchemaID != historysearch.EntryLookupSchemaID ||
		result.Entry.LogicalDialogID != logicalDialogID || result.Entry.EntryID != entryID || !validHistoryEntry(result.Entry) ||
		result.LagMillis < 0 || result.LagMillis > historyreplica.MaximumSafeInt {
		return HistoryEntryLookup{}, historySearchContractFault()
	}
	if _, err := time.Parse(time.RFC3339Nano, result.ObservedAt); err != nil {
		return HistoryEntryLookup{}, historySearchContractFault()
	}
	return result, nil
}

func (c *Client) UpsertHistoryDialogMetadata(ctx context.Context, owner, logicalDialogID string, value historysearch.DialogMetadata) (historysearch.DialogMetadata, error) {
	if c.workerToken == "" || !actorPattern.MatchString(owner) || !uuidPattern.MatchString(logicalDialogID) ||
		!validHistoryDialogMetadata(value, false) {
		return historysearch.DialogMetadata{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, _, err := c.writeWithMethodLimit(ctx, http.MethodPut, owner, "/internal/v1/history-search/dialogs/"+
		url.PathEscape(logicalDialogID)+"/metadata", value, 64<<10, http.StatusOK)
	if err != nil {
		return historysearch.DialogMetadata{}, err
	}
	var result historysearch.DialogMetadata
	if !decodeHostResponse(body, &result) || result.LogicalDialogID != logicalDialogID ||
		!validHistoryDialogMetadata(result, true) || result.NodeID != value.NodeID || result.NodeDialogID != value.NodeDialogID ||
		result.BindingGeneration != value.BindingGeneration {
		return historysearch.DialogMetadata{}, historySearchContractFault()
	}
	return result, nil
}

func (c *Client) HistoryReceiptByOrigin(ctx context.Context, owner, nodeID, commandID string) (historyreplica.ReceiptLookup, error) {
	if !actorPattern.MatchString(owner) || !uuidPattern.MatchString(nodeID) || !uuidPattern.MatchString(commandID) {
		return historyreplica.ReceiptLookup{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, err := c.read(ctx, owner, "/internal/v1/history-replica/receipts/"+url.PathEscape(nodeID)+"/"+url.PathEscape(commandID))
	if err != nil {
		return historyreplica.ReceiptLookup{}, err
	}
	var result historyreplica.ReceiptLookup
	if !decodeHostResponse(body, &result) || result.SchemaID != historyreplica.ReceiptSchemaID ||
		result.Receipt.Origin.NodeID != nodeID || result.Receipt.CommandID != commandID {
		return historyreplica.ReceiptLookup{}, historyContractFault()
	}
	return result, nil
}

func (c *Client) HistoryTextManifest(ctx context.Context, owner, logicalDialogID, textID string) (historyreplica.TextManifest, error) {
	if !actorPattern.MatchString(owner) || !uuidPattern.MatchString(logicalDialogID) || !uuidPattern.MatchString(textID) {
		return historyreplica.TextManifest{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, err := c.read(ctx, owner, "/internal/v1/history-replica/texts/"+url.PathEscape(logicalDialogID)+"/"+url.PathEscape(textID))
	if err != nil {
		return historyreplica.TextManifest{}, err
	}
	var result historyreplica.TextManifest
	if !decodeHostResponse(body, &result) || result.LogicalDialogID != logicalDialogID || result.TextID != textID {
		return historyreplica.TextManifest{}, historyContractFault()
	}
	return result, nil
}

func (c *Client) HistoryTextChunk(ctx context.Context, owner, logicalDialogID, textID string, index int64) (historyreplica.TextChunk, error) {
	if !actorPattern.MatchString(owner) || !uuidPattern.MatchString(logicalDialogID) || !uuidPattern.MatchString(textID) ||
		index < 0 || index > historyreplica.MaximumSafeInt {
		return historyreplica.TextChunk{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, err := c.read(ctx, owner, "/internal/v1/history-replica/texts/"+url.PathEscape(logicalDialogID)+"/"+
		url.PathEscape(textID)+"/chunks/"+strconv.FormatInt(index, 10))
	if err != nil {
		return historyreplica.TextChunk{}, err
	}
	var result historyreplica.TextChunk
	if !decodeHostResponse(body, &result) || result.LogicalDialogID != logicalDialogID || result.TextID != textID ||
		result.ChunkIndex != index || result.SizeBytes != int64(len(result.Bytes)) ||
		result.SizeBytes > historyreplica.MaximumChunkBytes || historyreplica.HashBytes(result.Bytes) != result.SHA256 {
		return historyreplica.TextChunk{}, historyContractFault()
	}
	return result, nil
}

func (c *Client) Hosts(ctx context.Context, owner string, limit int, cursor string) (DockerHostPage, error) {
	if !actorPattern.MatchString(owner) || limit < 1 || limit > 100 || len(cursor) > 512 {
		return DockerHostPage{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	body, err := c.read(ctx, owner, "/internal/v1/hosts?"+query.Encode())
	if err != nil {
		return DockerHostPage{}, err
	}
	var page DockerHostPage
	if !decodeHostResponse(body, &page) || !validDockerHostPage(page) {
		return DockerHostPage{}, hostContractFault()
	}
	return page, nil
}

func (c *Client) Host(ctx context.Context, owner, hostID string) (DockerHost, error) {
	if !actorPattern.MatchString(owner) || !uuidPattern.MatchString(hostID) {
		return DockerHost{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, err := c.read(ctx, owner, "/internal/v1/hosts/"+hostID)
	if err != nil {
		return DockerHost{}, err
	}
	var host DockerHost
	if !decodeHostResponse(body, &host) || !validDockerHost(host) || host.HostID != hostID {
		return DockerHost{}, hostContractFault()
	}
	return host, nil
}

func (c *Client) UpsertHost(ctx context.Context, owner string, input HostUpsert) (DockerHost, int, error) {
	if !actorPattern.MatchString(owner) || !validHostUpsert(input) {
		return DockerHost{}, 0, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, status, err := c.write(ctx, owner, "/internal/v1/hosts", input, http.StatusOK, http.StatusCreated)
	if err != nil {
		return DockerHost{}, 0, err
	}
	var host DockerHost
	if !decodeHostResponse(body, &host) || !validDockerHost(host) || host.HostID != input.HostID {
		return DockerHost{}, 0, hostContractFault()
	}
	return host, status, nil
}

func (c *Client) ProvisionHostSecret(ctx context.Context, owner string, input HostSecretInput) (HostSecretProvision, error) {
	if !actorPattern.MatchString(owner) || !validHostSecretInput(input) {
		return HostSecretProvision{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, status, err := c.write(ctx, owner, "/internal/v1/host-secrets", input, http.StatusCreated, http.StatusOK)
	if err != nil {
		return HostSecretProvision{}, err
	}
	defer zeroBytes(body)
	var provision HostSecretProvision
	if !decodeHostResponse(body, &provision) || !validHostSecretProvision(provision, input.OperationID) || provision.Kind != input.Kind {
		return HostSecretProvision{}, hostContractFault()
	}
	provision.Created = status == http.StatusCreated
	return provision, nil
}

func (c *Client) HostSecretProvision(ctx context.Context, owner, operationID string) (HostSecretProvision, error) {
	if !actorPattern.MatchString(owner) || !uuidPattern.MatchString(operationID) {
		return HostSecretProvision{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, err := c.read(ctx, owner, "/internal/v1/host-secrets/"+operationID)
	if err != nil {
		return HostSecretProvision{}, err
	}
	defer zeroBytes(body)
	var provision HostSecretProvision
	if !decodeHostResponse(body, &provision) || !validHostSecretProvision(provision, operationID) {
		return HostSecretProvision{}, hostContractFault()
	}
	return provision, nil
}

func (c *Client) ProbeHost(ctx context.Context, owner, hostID string, input HostProbeRequest) (DockerHost, error) {
	if !actorPattern.MatchString(owner) || !uuidPattern.MatchString(hostID) || input.SchemaID != HostProbeSchema ||
		input.ExpectedHostVersion < 1 || input.ExpectedHostVersion > 1<<53-1 {
		return DockerHost{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	client := c.probeHTTP
	if client == nil {
		client = c.http
	}
	body, _, err := c.writeWithClient(ctx, client, owner, "/internal/v1/hosts/"+hostID+"/probe", input, http.StatusOK)
	if err != nil {
		return DockerHost{}, err
	}
	var host DockerHost
	if !decodeHostResponse(body, &host) || !validDockerHost(host) || host.HostID != hostID || host.HostVersion != input.ExpectedHostVersion {
		return DockerHost{}, hostContractFault()
	}
	return host, nil
}

func (c *Client) ConfigurationDraft(ctx context.Context, owner, nodeID string) (ConfigurationDraft, error) {
	if !actorPattern.MatchString(owner) || !uuidPattern.MatchString(nodeID) {
		return ConfigurationDraft{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, err := c.readWithLimit(ctx, owner, "/internal/v1/configuration-drafts/"+nodeID, maximumConfigurationBody)
	if err != nil {
		return ConfigurationDraft{}, err
	}
	var draft ConfigurationDraft
	if !decodeHostResponse(body, &draft) || !validConfigurationDraft(draft, nodeID) {
		return ConfigurationDraft{}, configurationFault()
	}
	return draft, nil
}
func (c *Client) ValidateConfiguration(ctx context.Context, owner string, input ConfigurationInput) (ConfigurationValidation, error) {
	if !actorPattern.MatchString(owner) || input.SchemaID != ConfigurationValidateSchema {
		return ConfigurationValidation{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, _, err := c.writeWithLimit(ctx, owner, "/internal/v1/configuration-drafts/validate", input, maximumConfigurationBody, http.StatusOK)
	if err != nil {
		return ConfigurationValidation{}, err
	}
	var value ConfigurationValidation
	if !decodeHostResponse(body, &value) || value.SchemaID != ConfigurationValidateSchema || !validBuildContextManifest(value.BuildContextManifest) || !validValidation(value.Validation) {
		return ConfigurationValidation{}, configurationFault()
	}
	return value, nil
}
func (c *Client) SaveConfigurationDraft(ctx context.Context, owner, nodeID string, input ConfigurationSave) (ConfigurationDraft, *ConfigurationDraft, error) {
	if !actorPattern.MatchString(owner) || !uuidPattern.MatchString(nodeID) || input.SchemaID != ConfigurationSaveSchema {
		return ConfigurationDraft{}, nil, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return ConfigurationDraft{}, nil, configurationFault()
	}
	if len(raw) > maximumConfigurationBody {
		zeroBytes(raw)
		return ConfigurationDraft{}, nil, configurationFault()
	}
	defer zeroBytes(raw)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent-service/internal/v1/configuration-drafts/"+nodeID, bytes.NewReader(raw))
	if err != nil {
		return ConfigurationDraft{}, nil, configurationFault()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(OwnerHeader, owner)
	response, err := c.http.Do(req)
	if err != nil {
		return ConfigurationDraft{}, nil, &Fault{Status: http.StatusServiceUnavailable, Code: "configuration_unavailable", Retryable: true}
	}
	defer response.Body.Close()
	mediaType, _, contentTypeErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumConfigurationBody+1))
	if contentTypeErr != nil || mediaType != "application/json" || readErr != nil || len(body) > maximumConfigurationBody || !strictjson.Valid(body) {
		return ConfigurationDraft{}, nil, configurationFault()
	}
	if response.StatusCode == http.StatusOK {
		var draft ConfigurationDraft
		if !decodeHostResponse(body, &draft) || !validConfigurationDraft(draft, nodeID) {
			return ConfigurationDraft{}, nil, configurationFault()
		}
		return draft, nil, nil
	}
	if response.StatusCode == http.StatusConflict {
		var conflict struct {
			SchemaID string             `json:"schemaId"`
			Error    string             `json:"error"`
			Current  ConfigurationDraft `json:"current"`
		}
		if !decodeHostResponse(body, &conflict) || conflict.SchemaID != "agent-configuration-conflict-v2" || conflict.Error != "configuration_version_conflict" || !validConfigurationDraft(conflict.Current, nodeID) {
			return ConfigurationDraft{}, nil, configurationFault()
		}
		return ConfigurationDraft{}, &conflict.Current, &Fault{Status: http.StatusConflict, Code: conflict.Error}
	}
	return ConfigurationDraft{}, nil, &Fault{Status: response.StatusCode, Code: "configuration_unavailable", Retryable: response.StatusCode >= 500}
}

func (c *Client) PrepareExternalEnrollment(ctx context.Context, owner string, input ExternalEnrollmentRequest) (ExternalEnrollmentPlan, error) {
	if c.workerToken == "" || !actorPattern.MatchString(owner) || input.SchemaID != ExternalEnrollmentSchema ||
		!actorPattern.MatchString(input.OperationID) || !uuidPattern.MatchString(input.HostID) ||
		!uuidPattern.MatchString(input.NodeID) || input.ExpectedHostVersion < 1 ||
		input.ExpectedHostVersion > 1<<53-1 || !bounded(input.Name, 200) ||
		(input.Adapter != "cursor" && input.Adapter != "codex") || !sha256Pattern.MatchString(input.CertificateSHA256) ||
		len(input.EndpointURI) == 0 || len(input.EndpointURI) > 512 {
		return ExternalEnrollmentPlan{}, &Fault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	body, _, err := c.write(ctx, owner, "/internal/v1/external-enrollments", input, http.StatusAccepted)
	if err != nil {
		return ExternalEnrollmentPlan{}, err
	}
	var plan ExternalEnrollmentPlan
	if !decodeHostResponse(body, &plan) || !validExternalEnrollmentPlan(plan, input) {
		return ExternalEnrollmentPlan{}, &Fault{Status: http.StatusServiceUnavailable, Code: "invalid_enrollment_response", Retryable: true}
	}
	return plan, nil
}

func (c *Client) RegistryOperationSent(ctx context.Context, owner string, status RegistryOperationStatus) (RegistryOperationStatus, error) {
	return c.transitionRegistryOperation(ctx, owner, "/internal/v1/registry-operations/sent", status)
}

func (c *Client) RegistryOperationUnknown(ctx context.Context, owner string, status RegistryOperationStatus) (RegistryOperationStatus, error) {
	return c.transitionRegistryOperation(ctx, owner, "/internal/v1/registry-operations/unknown", status)
}

func (c *Client) transitionRegistryOperation(ctx context.Context, owner, endpoint string, status RegistryOperationStatus) (RegistryOperationStatus, error) {
	command := RegistryOperationCommand{SchemaID: RegistryCommandSchema, OperationID: status.Receipt.OperationID, RequestHash: status.Receipt.RequestHash, OperationVersion: status.OperationVersion}
	body, _, err := c.write(ctx, owner, endpoint, command, http.StatusOK)
	if err != nil {
		return RegistryOperationStatus{}, err
	}
	var next RegistryOperationStatus
	if !decodeHostResponse(body, &next) || !validRegistryOperationStatus(next, status.Receipt.OperationID) {
		return RegistryOperationStatus{}, &Fault{Status: http.StatusServiceUnavailable, Code: "invalid_enrollment_response", Retryable: true}
	}
	return next, nil
}

func (c *Client) FinishRegistryOperation(ctx context.Context, owner string, status RegistryOperationStatus, effectState string) (RegistryOperationStatus, error) {
	finish := RegistryOperationFinish{
		SchemaID: RegistryFinishSchema, OperationID: status.Receipt.OperationID, RequestHash: status.Receipt.RequestHash,
		OperationVersion: status.OperationVersion, RegistryVersion: status.Receipt.CandidateRegistryVersion,
		RegistrySHA256: status.Receipt.CandidateRegistrySHA256, EffectState: effectState,
	}
	body, _, err := c.write(ctx, owner, "/internal/v1/registry-operations/finish", finish, http.StatusOK)
	if err != nil {
		return RegistryOperationStatus{}, err
	}
	var next RegistryOperationStatus
	if !decodeHostResponse(body, &next) || !validRegistryOperationStatus(next, status.Receipt.OperationID) || next.Phase != "succeeded" {
		return RegistryOperationStatus{}, &Fault{Status: http.StatusServiceUnavailable, Code: "invalid_enrollment_response", Retryable: true}
	}
	return next, nil
}

func (c *Client) FailRegistryOperation(ctx context.Context, owner string, status RegistryOperationStatus, resultCode string) (RegistryOperationStatus, error) {
	failure := RegistryOperationFailure{
		SchemaID: RegistryFailureSchema, OperationID: status.Receipt.OperationID, RequestHash: status.Receipt.RequestHash,
		OperationVersion: status.OperationVersion, ResultCode: resultCode,
	}
	body, _, err := c.write(ctx, owner, "/internal/v1/registry-operations/fail", failure, http.StatusOK)
	if err != nil {
		return RegistryOperationStatus{}, err
	}
	var next RegistryOperationStatus
	if !decodeHostResponse(body, &next) || !validRegistryOperationStatus(next, status.Receipt.OperationID) || next.Phase != "failed" {
		return RegistryOperationStatus{}, &Fault{Status: http.StatusServiceUnavailable, Code: "invalid_enrollment_response", Retryable: true}
	}
	return next, nil
}

func validExternalEnrollmentPlan(plan ExternalEnrollmentPlan, request ExternalEnrollmentRequest) bool {
	if plan.SchemaID != ExternalEnrollmentPlanSchema || plan.OperationID != request.OperationID || plan.NodeID != request.NodeID ||
		plan.RequestHash != externalEnrollmentRequestHash(request) || len(plan.Registry) == 0 || len(plan.Registry) > 256<<10 ||
		!strictjson.Valid(plan.Registry) || !validRegistryOperationStatus(plan.Status, request.OperationID) ||
		len(plan.Status.Receipt.AffectedNodeIDs) != 1 || plan.Status.Receipt.AffectedNodeIDs[0] != request.NodeID {
		return false
	}
	if plan.Binding == nil {
		return plan.Status.Phase == "succeeded"
	}
	binding := plan.Binding
	return binding.Kind == "external" && binding.NodeID == request.NodeID && binding.RegistrationRevision == 1 &&
		binding.RegistrationEpoch == 1 && binding.EndpointRevision == 1 && binding.HostID == request.HostID &&
		binding.HostVersion == request.ExpectedHostVersion && oneOf(binding.Transport, "local", "ssh") &&
		actorPattern.MatchString(binding.TargetRef) && binding.DockerContextRef == "" && binding.ExpectedHostIdentitySHA256 == "" &&
		binding.HostPlatform == "" && binding.HostArchitecture == "" && binding.ContainerID == "" && binding.RuntimeGeneration == 0 &&
		binding.Address != "" && ((binding.Transport == "local" && binding.CredentialRef == "" && binding.ExpectedHostKey == "") ||
		(binding.Transport == "ssh" && actorPattern.MatchString(binding.CredentialRef) && hostKeyPattern.MatchString(binding.ExpectedHostKey)))
}

func externalEnrollmentRequestHash(request ExternalEnrollmentRequest) string {
	raw, err := json.Marshal(request)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func validRegistryOperationStatus(value RegistryOperationStatus, operationID string) bool {
	receipt := value.Receipt
	if value.SchemaID != "agent-registry-operation-status-v1" || receipt.SchemaID != "agent-registry-operation-receipt-v1" ||
		receipt.OperationID != operationID || !actorPattern.MatchString(operationID) || !sha256Pattern.MatchString(receipt.RequestHash) ||
		receipt.ExpectedRegistryVersion < 1 || receipt.CandidateRegistryVersion != receipt.ExpectedRegistryVersion+1 ||
		!sha256Pattern.MatchString(receipt.ExpectedRegistrySHA256) || !sha256Pattern.MatchString(receipt.CandidateRegistrySHA256) ||
		receipt.AffectedNodeIDs == nil || len(receipt.AffectedNodeIDs) == 0 || value.OperationVersion < 1 ||
		!oneOf(value.Phase, "accepted", "applying", "reconciling", "succeeded", "failed") ||
		!oneOf(value.EffectState, "not_sent", "sent", "unknown", "acknowledged", "reconciled", "failed") {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, receipt.AcceptedAt); err != nil {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, value.UpdatedAt); err != nil {
		return false
	}
	seen := map[string]bool{}
	for _, nodeID := range receipt.AffectedNodeIDs {
		if !uuidPattern.MatchString(nodeID) || seen[nodeID] {
			return false
		}
		seen[nodeID] = true
	}
	if value.Phase == "failed" {
		return value.EffectState == "failed" && value.ResultCode != nil && actorPattern.MatchString(*value.ResultCode)
	}
	if value.ResultCode != nil {
		return false
	}
	return value.Phase == "accepted" && value.EffectState == "not_sent" ||
		value.Phase == "applying" && value.EffectState == "sent" ||
		value.Phase == "reconciling" && value.EffectState == "unknown" ||
		value.Phase == "succeeded" && oneOf(value.EffectState, "acknowledged", "reconciled")
}

func validValidation(value ConfigurationValidationState) bool {
	if value.EffectStatus != "none" || value.Diagnostics == nil || value.Valid != (len(value.Diagnostics) == 0) {
		return false
	}
	for _, d := range value.Diagnostics {
		if d.Code == "" || d.Line < 1 || d.Column < 1 || len(d.Pointer) > 512 || d.Message == "" {
			return false
		}
	}
	return true
}
func validConfigurationDraft(value ConfigurationDraft, nodeID string) bool {
	return value.SchemaID == ConfigurationDraftSchema && value.NodeID == nodeID && value.DraftVersion >= 1 && value.DraftVersion <= 1<<53-1 && utf8.ValidString(value.RawJSONText) && len(value.RawJSONText) <= 256<<10 && utf8.ValidString(value.RawDockerfileText) && len(value.RawDockerfileText) <= 128<<10 && validBuildContextManifest(value.BuildContextManifest) && validValidation(value.Validation)
}
func validBuildContextManifest(manifest *BuildContextManifest) bool {
	if manifest == nil {
		return true
	}
	if !uuidPattern.MatchString(manifest.Revision) || manifest.Assets == nil || len(manifest.Assets) > 1000 {
		return false
	}
	paths := map[string]bool{}
	for _, asset := range manifest.Assets {
		if !validBuildContextPath(asset.Path) || !uuidPattern.MatchString(asset.AssetID) || !sha256Pattern.MatchString(asset.SHA256) || paths[asset.Path] {
			return false
		}
		paths[asset.Path] = true
	}
	return true
}
func validBuildContextPath(value string) bool {
	if len(value) < 1 || len(value) > 256 || !utf8.ValidString(value) || value[0] == '/' || strings.Contains(value, "\\") || path.Clean(value) != value {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, character := range segment {
			if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-') {
				return false
			}
		}
	}
	return true
}
func configurationFault() *Fault {
	return &Fault{Status: http.StatusServiceUnavailable, Code: "invalid_configuration_response", Retryable: true}
}

func (c *Client) read(ctx context.Context, owner, path string) ([]byte, error) {
	return c.readWithLimit(ctx, owner, path, maximumBody)
}

func (c *Client) readWithLimit(ctx context.Context, owner, path string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent-service"+path, nil)
	if err != nil {
		return nil, &Fault{Status: http.StatusServiceUnavailable, Code: "inventory_unavailable", Retryable: true}
	}
	request.Header.Set(OwnerHeader, owner)
	if c.workerToken != "" && workerScopedPath(path) {
		request.Header.Set("X-Agent-Service-Worker-Token", c.workerToken)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, &Fault{Status: http.StatusServiceUnavailable, Code: "inventory_unavailable", Retryable: true}
	}
	defer response.Body.Close()
	mediaType, _, contentTypeErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	body, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if contentTypeErr != nil || mediaType != "application/json" || readErr != nil || len(body) > int(limit) || !strictjson.Valid(body) {
		return nil, &Fault{Status: http.StatusServiceUnavailable, Code: "invalid_inventory_response", Retryable: true}
	}
	if response.StatusCode == http.StatusOK {
		return body, nil
	}
	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil || envelope.Error.Code == "" {
		return nil, &Fault{Status: http.StatusServiceUnavailable, Code: "invalid_inventory_response", Retryable: true}
	}
	status := response.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusServiceUnavailable
	}
	return nil, &Fault{Status: status, Code: safeCode(envelope.Error.Code), Retryable: envelope.Error.Retryable}
}

func (c *Client) write(ctx context.Context, owner, path string, input any, successful ...int) ([]byte, int, error) {
	return c.writeWithClient(ctx, c.http, owner, path, input, successful...)
}

func (c *Client) writeWithLimit(ctx context.Context, owner, path string, input any, limit int, successful ...int) ([]byte, int, error) {
	return c.writeWithClientLimit(ctx, c.http, owner, path, input, limit, successful...)
}

func (c *Client) writeWithMethodLimit(ctx context.Context, method, owner, path string, input any, limit int, successful ...int) ([]byte, int, error) {
	return c.writeWithMethodClientLimit(ctx, c.http, method, owner, path, input, limit, successful...)
}

func (c *Client) writeWithClient(ctx context.Context, client *http.Client, owner, path string, input any, successful ...int) ([]byte, int, error) {
	return c.writeWithClientLimit(ctx, client, owner, path, input, maximumBody, successful...)
}

func (c *Client) writeWithClientLimit(ctx context.Context, client *http.Client, owner, path string, input any, limit int, successful ...int) ([]byte, int, error) {
	return c.writeWithMethodClientLimit(ctx, client, http.MethodPost, owner, path, input, limit, successful...)
}

func (c *Client) writeWithMethodClientLimit(ctx context.Context, client *http.Client, method, owner, path string, input any, limit int, successful ...int) ([]byte, int, error) {
	raw, err := json.Marshal(input)
	if err != nil || len(raw) == 0 || len(raw) > limit {
		zeroBytes(raw)
		return nil, 0, hostContractFault()
	}
	defer zeroBytes(raw)
	request, err := http.NewRequestWithContext(ctx, method, "http://agent-service"+path, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, &Fault{Status: http.StatusServiceUnavailable, Code: "hosts_unavailable", Retryable: true}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(OwnerHeader, owner)
	if c.workerToken != "" && workerScopedPath(path) {
		request.Header.Set("X-Agent-Service-Worker-Token", c.workerToken)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, &Fault{Status: http.StatusServiceUnavailable, Code: "hosts_unavailable", Retryable: true}
	}
	defer response.Body.Close()
	mediaType, _, contentTypeErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	body, readErr := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if contentTypeErr != nil || mediaType != "application/json" || readErr != nil || len(body) == 0 || len(body) > limit || !strictjson.Valid(body) {
		zeroBytes(body)
		return nil, 0, hostContractFault()
	}
	for _, status := range successful {
		if response.StatusCode == status {
			return body, response.StatusCode, nil
		}
	}
	defer zeroBytes(body)
	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil || decoder.Decode(new(any)) != io.EOF || envelope.Error.Code == "" {
		return nil, 0, hostContractFault()
	}
	status := response.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusServiceUnavailable
	}
	return nil, 0, &Fault{Status: status, Code: safeCode(envelope.Error.Code), Retryable: envelope.Error.Retryable}
}

func decodeHostResponse(body []byte, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target) == nil && decoder.Decode(new(any)) == io.EOF
}

func hostContractFault() *Fault {
	return &Fault{Status: http.StatusServiceUnavailable, Code: "invalid_hosts_response", Retryable: true}
}

func historyContractFault() *Fault {
	return &Fault{Status: http.StatusServiceUnavailable, Code: "invalid_history_response", Retryable: true}
}

func historySearchContractFault() *Fault {
	return &Fault{Status: http.StatusServiceUnavailable, Code: "invalid_history_search_response", Retryable: true}
}

func workerScopedPath(path string) bool {
	return path == "/internal/v1/external-enrollments" || path == "/internal/v1/history-replica/batches" ||
		strings.HasPrefix(path, "/internal/v1/history-search/dialogs/") ||
		strings.HasPrefix(path, "/internal/v1/registry-operations/") ||
		strings.HasPrefix(path, "/internal/v1/logical-dialog-deletes")
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func safeCode(value string) string {
	switch value {
	case "invalid_request", "invalid_cursor", "owner_scope_required", "not_found",
		"database_unavailable", "inventory_unavailable", "hosts_unavailable", "invalid_hosts_response",
		"host_version_conflict", "host_adapter_not_configured", "host_probe_unavailable", "secret_store_unavailable",
		"secret_operation_conflict", "secret_provision_not_found", "probe_superseded", "configuration_draft_not_found", "configuration_version_conflict", "configuration_unavailable", "invalid_configuration_response":
		return value
	case "worker_scope_required", "registry_operations_unavailable", "registry_operation_conflict", "registry_operation_unavailable",
		"enrollment_unavailable", "endpoint_rejected", "enrollment_conflict", "registry_not_found", "invalid_enrollment_response",
		"admission_profile_missing", "admission_schema_mismatch", "admission_owner_mismatch", "admission_identity_mismatch",
		"admission_capability_missing", "admission_unready", "node_not_ready", "state_unavailable", "tunnel_not_configured",
		"registry_projection_required", "invalid_history_batch", "history_gap", "history_hash_conflict",
		"history_replica_unavailable", "invalid_history_response", "logical_delete_conflict", "history_not_ready",
		"logical_delete_unavailable", "invalid_logical_delete_response", "history_search_unavailable",
		"invalid_history_search_response", "search_snapshot_expired":
		return value
	default:
		return "inventory_unavailable"
	}
}

func validHistorySearchQuery(query historysearch.Query) bool {
	return utf8.ValidString(query.Q) && query.Q != "" && len([]byte(query.Q)) <= historysearch.MaximumQueryBytes &&
		!strings.ContainsRune(query.Q, '\x00') && (query.NodeID == "" || uuidPattern.MatchString(query.NodeID)) &&
		(query.LogicalDialogID == "" || uuidPattern.MatchString(query.LogicalDialogID))
}

func normalizedHistoryQuery(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

func validHistorySearchPage(page historysearch.Page, query historysearch.Query, limit int) bool {
	if page.SchemaID != historysearch.SchemaID || !validHistorySearchQuery(page.Query) ||
		normalizedHistoryQuery(page.Query.Q) != normalizedHistoryQuery(query.Q) ||
		page.Query.NodeID != query.NodeID || page.Query.LogicalDialogID != query.LogicalDialogID ||
		(page.Query.Archived == nil) != (query.Archived == nil) ||
		(page.Query.Archived != nil && *page.Query.Archived != *query.Archived) || page.Items == nil || len(page.Items) > limit ||
		page.TotalCount < 0 || page.TotalCount > historyreplica.MaximumSafeInt || page.LagMillis < 0 ||
		page.LagMillis > historyreplica.MaximumSafeInt ||
		(page.NextCursor != nil && (*page.NextCursor == "" || len(*page.NextCursor) > 8192)) {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, page.ObservedAt); err != nil {
		return false
	}
	seen := make(map[string]bool, len(page.Items))
	for _, item := range page.Items {
		key := item.LogicalDialogID + ":" + item.EntryID
		if seen[key] || !uuidPattern.MatchString(item.LogicalDialogID) || !uuidPattern.MatchString(item.EntryID) ||
			!uuidPattern.MatchString(item.NodeID) || !uuidPattern.MatchString(item.NodeDialogID) ||
			!utf8.ValidString(item.Title) || len([]byte(item.Title)) > historysearch.MaximumTitleBytes || strings.ContainsRune(item.Title, '\x00') ||
			(item.Role != "user" && item.Role != "assistant") || !bounded(item.Kind, 120) ||
			!utf8.ValidString(item.Snippet) || len([]byte(item.Snippet)) > historysearch.MaximumSnippetBytes || strings.ContainsRune(item.Snippet, '\x00') ||
			math.IsNaN(float64(item.Rank)) || math.IsInf(float64(item.Rank), 0) || item.Rank < 0 {
			return false
		}
		if _, err := time.Parse(time.RFC3339Nano, item.CreatedAt); err != nil {
			return false
		}
		seen[key] = true
	}
	return true
}

func validHistoryDialogMetadata(value historysearch.DialogMetadata, response bool) bool {
	logicalValid := value.LogicalDialogID == ""
	if response {
		logicalValid = uuidPattern.MatchString(value.LogicalDialogID)
	}
	if value.SchemaID != historysearch.MetadataSchemaID || !logicalValid || !uuidPattern.MatchString(value.NodeID) ||
		!uuidPattern.MatchString(value.NodeDialogID) || value.BindingGeneration < 1 ||
		value.BindingGeneration > historyreplica.MaximumSafeInt || !utf8.ValidString(value.Title) ||
		len([]byte(value.Title)) > historysearch.MaximumTitleBytes || strings.ContainsRune(value.Title, '\x00') {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, value.UpdatedAt)
	return err == nil
}

func validHistoryEntry(entry historyreplica.TranscriptEntry) bool {
	if !uuidPattern.MatchString(entry.EntryID) || !uuidPattern.MatchString(entry.MessageID) ||
		!uuidPattern.MatchString(entry.LogicalDialogID) || !uuidPattern.MatchString(entry.Origin.NodeID) ||
		!uuidPattern.MatchString(entry.Origin.NodeDialogID) || !bounded(entry.Origin.AuthorEventID, 256) ||
		(entry.RequestID != "" && !uuidPattern.MatchString(entry.RequestID)) ||
		(entry.AttemptID != "" && !uuidPattern.MatchString(entry.AttemptID)) || !bounded(entry.Kind, 120) ||
		(entry.Role != "user" && entry.Role != "assistant") || entry.SourceGeneration < 0 ||
		entry.SourceGeneration > historyreplica.MaximumSafeInt || entry.ExecutionOrdinal < 1 ||
		entry.ExecutionOrdinal > historyreplica.MaximumSafeInt || !bounded(entry.ExecutionStatus, 120) ||
		(entry.ProfileHash != "" && !sha256Pattern.MatchString(entry.ProfileHash)) || !strictjson.Valid(entry.Content) {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, entry.CreatedAt); err != nil {
		return false
	}
	return validHistoryEntryContent(entry.Content, entry.Role)
}

func validHistoryEntryContent(raw json.RawMessage, role string) bool {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&fields) != nil || decoder.Decode(new(any)) != io.EOF || fields == nil {
		return false
	}
	var kind string
	if json.Unmarshal(fields["kind"], &kind) != nil {
		return false
	}
	allowed := func(names ...string) bool {
		if len(fields) != len(names) {
			return false
		}
		for _, name := range names {
			if _, ok := fields[name]; !ok {
				return false
			}
		}
		return true
	}
	if role == "user" {
		var content string
		return kind == "inline" && allowed("kind", "content") && json.Unmarshal(fields["content"], &content) == nil &&
			utf8.ValidString(content) && len([]byte(content)) <= 64<<10 && !strings.ContainsRune(content, '\x00')
	}
	var redaction string
	var truncated bool
	if json.Unmarshal(fields["redaction"], &redaction) != nil || json.Unmarshal(fields["truncated"], &truncated) != nil {
		return false
	}
	switch kind {
	case "inline":
		var content string
		return allowed("kind", "content", "redaction", "truncated") && json.Unmarshal(fields["content"], &content) == nil &&
			utf8.ValidString(content) && len([]byte(content)) <= 64<<10 && !strings.ContainsRune(content, '\x00') &&
			(redaction == "none" || redaction == "applied")
	case "artifact":
		var artifactID, hash string
		var size int64
		return allowed("kind", "artifactId", "sizeBytes", "sha256", "redaction", "truncated") &&
			json.Unmarshal(fields["artifactId"], &artifactID) == nil && uuidPattern.MatchString(artifactID) &&
			json.Unmarshal(fields["sizeBytes"], &size) == nil && size >= 0 && size <= historyreplica.MaximumSafeInt &&
			json.Unmarshal(fields["sha256"], &hash) == nil && sha256Pattern.MatchString(hash) &&
			(redaction == "none" || redaction == "applied")
	case "unavailable":
		var reason string
		return allowed("kind", "reason", "redaction", "truncated") && json.Unmarshal(fields["reason"], &reason) == nil &&
			oneOf(reason, "not_observed", "provider_redacted", "output_limit", "unmapped") &&
			oneOf(redaction, "none", "applied", "unknown")
	default:
		return false
	}
}

func validPage(page Page) bool {
	if page.SchemaID != InventorySchema || page.Items == nil || len(page.Items) > 100 ||
		(page.NextCursor != nil && (*page.NextCursor == "" || len(*page.NextCursor) > 512)) {
		return false
	}
	ids := map[string]bool{}
	for _, item := range page.Items {
		if !uuidPattern.MatchString(item.NodeID) || ids[item.NodeID] || !bounded(item.Name, 200) ||
			(item.Engine != "cursor" && item.Engine != "codex") ||
			(item.SourceMode != "live" && item.SourceMode != "fixture") ||
			!uuidPattern.MatchString(item.Host.HostID) || !bounded(item.Host.Name, 200) ||
			(item.RegistrationMode != "legacy_readonly" && item.RegistrationMode != "compatible") ||
			!oneOf(item.Status, "online", "unready", "busy", "stale", "stopped", "unknown", "readonly") ||
			!oneOf(item.State.Process, "running", "stopped", "unknown") ||
			!oneOf(item.State.Connection, "online", "offline", "unknown") ||
			!oneOf(item.State.Readiness, "ready", "unready", "unknown") ||
			!oneOf(item.State.Occupancy, "idle", "busy", "unknown") ||
			item.DialogCount < 0 || item.DialogCount > 1<<53-1 || !validActions(item.Actions) {
			return false
		}
		ids[item.NodeID] = true
		if (item.ObservedAt == nil) != (item.Source == nil) {
			return false
		}
		if item.ObservedAt != nil {
			if _, err := time.Parse(time.RFC3339Nano, *item.ObservedAt); err != nil || !bounded(*item.Source, 100) {
				return false
			}
		}
		if item.PendingCount != nil {
			if item.PendingCount.Value < 0 || item.PendingCount.Value > 1<<53-1 || !bounded(item.PendingCount.Source, 100) ||
				item.ObservedAt == nil || item.Source == nil ||
				item.PendingCount.ObservedAt != *item.ObservedAt || item.PendingCount.Source != *item.Source {
				return false
			}
		}
	}
	return true
}

func validDialogPage(page DialogPage, nodeID string) bool {
	if page.SchemaID != BindingsSchema || page.NodeID != nodeID || page.Items == nil || len(page.Items) > 100 ||
		(page.NextCursor != nil && (*page.NextCursor == "" || len(*page.NextCursor) > 512)) {
		return false
	}
	seen := map[string]bool{}
	for _, dialog := range page.Items {
		if !uuidPattern.MatchString(dialog.NodeDialogID) || seen[dialog.NodeDialogID] ||
			!uuidPattern.MatchString(dialog.LogicalDialogID) || dialog.BindingVersion < 1 || dialog.BindingVersion > 1<<53-1 {
			return false
		}
		seen[dialog.NodeDialogID] = true
	}
	return true
}

func validActions(actions Actions) bool {
	return validAction(actions.OpenWorkspace) && validAction(actions.SendMessage) && validAction(actions.Lifecycle)
}

func validAction(action Action) bool {
	if action.Allowed {
		return action.Reason == "" && action.NextAction == ""
	}
	return bounded(action.Reason, 100) && bounded(action.NextAction, 500)
}

func bounded(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && !strings.ContainsAny(value, "\x00\r\n")
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func validHostUpsert(value HostUpsert) bool {
	if value.SchemaID != HostUpsertSchema || !uuidPattern.MatchString(value.HostID) || value.ExpectedHostVersion < 0 ||
		value.ExpectedHostVersion > 1<<53-1 || !bounded(value.DisplayName, 120) || !actorPattern.MatchString(value.TargetRef) ||
		!actorPattern.MatchString(value.DockerContextRef) || !oneOf(value.Transport, "local", "ssh") ||
		!oneOf(value.HostPlatform, "linux", "darwin", "windows") || !oneOf(value.HostArchitecture, "amd64", "arm64") {
		return false
	}
	for _, ref := range []string{value.CredentialRef, value.RegistryCredentialRef} {
		if ref != "" && !actorPattern.MatchString(ref) {
			return false
		}
	}
	if value.ExpectedIdentitySHA256 != "" && !sha256Pattern.MatchString(value.ExpectedIdentitySHA256) {
		return false
	}
	if value.ExpectedHostKey != "" && !hostKeyPattern.MatchString(value.ExpectedHostKey) {
		return false
	}
	return value.Transport == "local" && value.CredentialRef == "" && value.ExpectedHostKey == "" ||
		value.Transport == "ssh" && value.CredentialRef != ""
}

func validHostSecretInput(value HostSecretInput) bool {
	if value.SchemaID != HostSecretSchema || !uuidPattern.MatchString(value.OperationID) {
		return false
	}
	if value.Kind == "ssh" {
		return len(value.PrivateKey) > 0 && len(value.PrivateKey) <= 64<<10 && len(value.Passphrase) <= 4<<10 && len(value.Payload) == 0
	}
	return value.Kind == "registry" && len(value.Payload) > 0 && len(value.Payload) <= 64<<10 && len(value.PrivateKey) == 0 && len(value.Passphrase) == 0
}

func validHostSecretProvision(value HostSecretProvision, operationID string) bool {
	return value.SchemaID == HostSecretProvisionSchema && value.OperationID == operationID && uuidPattern.MatchString(operationID) &&
		oneOf(value.Kind, "ssh", "registry") && value.Status == "provisioned" && actorPattern.MatchString(value.CredentialRef)
}

func validDockerHostPage(page DockerHostPage) bool {
	if page.SchemaID != HostPageSchema || page.Items == nil || len(page.Items) > 100 ||
		page.NextCursor != nil && (*page.NextCursor == "" || len(*page.NextCursor) > 512) {
		return false
	}
	seen := map[string]bool{}
	for _, host := range page.Items {
		if !validDockerHost(host) || seen[host.HostID] {
			return false
		}
		seen[host.HostID] = true
	}
	return true
}

func validDockerHost(host DockerHost) bool {
	upsert := HostUpsert{
		SchemaID: HostUpsertSchema, HostID: host.HostID, ExpectedHostVersion: host.HostVersion,
		DisplayName: host.DisplayName, Transport: host.Transport, TargetRef: host.TargetRef,
		CredentialRef: host.CredentialRef, RegistryCredentialRef: host.RegistryCredentialRef,
		ExpectedHostKey: host.ExpectedHostKey, DockerContextRef: host.DockerContextRef,
		ExpectedIdentitySHA256: host.ExpectedIdentitySHA256, HostPlatform: host.HostPlatform,
		HostArchitecture: host.HostArchitecture,
	}
	if host.SchemaID != HostSchema || host.HostVersion < 1 || !validHostUpsert(upsert) || host.Capabilities == nil ||
		len(host.Capabilities) > 32 || !oneOf(host.Availability, "unverified", "ready", "unavailable") ||
		!oneOf(host.RegistryAvailability, "not_configured", "not_checked", "ready", "unavailable") {
		return false
	}
	capabilities := map[string]bool{}
	for _, capability := range host.Capabilities {
		if !actorPattern.MatchString(capability) || capabilities[capability] {
			return false
		}
		capabilities[capability] = true
	}
	if host.ObservedAt != nil {
		if _, err := time.Parse(time.RFC3339Nano, *host.ObservedAt); err != nil {
			return false
		}
	}
	switch host.Availability {
	case "unverified":
		return host.ObservedAt == nil && host.FailureStage == "" && host.FailureCode == "" && host.IdentitySHA256 == ""
	case "ready":
		return host.ObservedAt != nil && host.FailureStage == "" && host.FailureCode == "" && host.NextAction == "" &&
			sha256Pattern.MatchString(host.IdentitySHA256) && bounded(host.DaemonID, 128) && bounded(host.ContextEndpoint, 80) &&
			host.EngineOS == "linux" && oneOf(host.Architecture, "amd64", "arm64") && bounded(host.APIVersion, 32) && bounded(host.EngineVersion, 64)
	case "unavailable":
		return host.ObservedAt != nil && actorPattern.MatchString(host.FailureStage) && actorPattern.MatchString(host.FailureCode) &&
			bounded(host.NextAction, 300) && host.IdentitySHA256 == ""
	}
	return false
}
