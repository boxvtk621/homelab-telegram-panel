// Package operationclient is the private R02 client used only by the separate
// adapter process. Panel never imports it and never receives worker leases.
package operationclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/dockeradapter"
	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

const (
	OwnerHeader          = "X-Agent-Service-Owner"
	WorkerTokenHeader    = "X-Agent-Service-Worker-Token"
	IntentSchemaID       = "agent-operation-v1"
	ReceiptSchemaID      = "agent-operation-receipt-v1"
	StatusSchemaID       = "agent-operation-status-v1"
	TargetSchemaID       = "agent-operation-target-v1"
	ClaimRequestSchemaID = "agent-operation-claim-request-v1"
	ProofSchemaID        = "agent-operation-proof-v1"
	WorkSchemaID         = "agent-operation-work-v1"
	AuthoritySchemaID    = "agent-operation-authority-v1"
	SentSchemaID         = "agent-operation-sent-v1"
	AdvanceSchemaID      = "agent-operation-advance-v1"
	maximumSafeInteger   = int64(1<<53 - 1)
	maximumOperationBody = 2 << 20
)

var (
	actor = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)
	uuid  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	hash  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Target struct {
	NodeID               string `json:"nodeId"`
	HostID               string `json:"hostId"`
	RegistrationRevision int64  `json:"registrationRevision"`
	RegistrationEpoch    int64  `json:"registrationEpoch"`
	Generation           int64  `json:"generation"`
}

type TargetStatus struct {
	SchemaID         string `json:"schemaId"`
	Target           Target `json:"target"`
	RegistryMode     string `json:"registryMode"`
	RegistrationMode string `json:"registrationMode"`
}

type Step struct {
	StepID      string   `json:"stepId"`
	Action      string   `json:"action"`
	ResourceIDs []string `json:"resourceIds"`
}

type Intent struct {
	SchemaID    string `json:"schemaId"`
	OperationID string `json:"operationId"`
	Kind        string `json:"kind"`
	Target      Target `json:"target"`
	Step        Step   `json:"step"`
}

type Receipt struct {
	SchemaID           string `json:"schemaId"`
	OperationID        string `json:"operationId"`
	RequestHash        string `json:"requestHash"`
	Target             Target `json:"target"`
	AcceptedGeneration int64  `json:"acceptedGeneration"`
	AcceptedAt         string `json:"acceptedAt"`
}

type Status struct {
	SchemaID         string  `json:"schemaId"`
	Receipt          Receipt `json:"receipt"`
	Phase            string  `json:"phase"`
	EffectState      string  `json:"effectState"`
	OperationVersion int64   `json:"operationVersion"`
	UpdatedAt        string  `json:"updatedAt"`
	ResultCode       *string `json:"resultCode"`
}

type Proof struct {
	SchemaID         string `json:"schemaId"`
	OperationID      string `json:"operationId"`
	RequestHash      string `json:"requestHash"`
	NodeID           string `json:"nodeId"`
	Generation       int64  `json:"generation"`
	WorkerID         string `json:"workerId"`
	WorkerToken      string `json:"workerToken"`
	OperationVersion int64  `json:"operationVersion"`
	LeaseExpiresAt   string `json:"leaseExpiresAt"`
}

type Work struct {
	SchemaID    string `json:"schemaId"`
	Intent      Intent `json:"intent"`
	Proof       Proof  `json:"proof"`
	EffectState string `json:"effectState"`
}

type claimRequest struct {
	SchemaID          string `json:"schemaId"`
	NodeID            string `json:"nodeId"`
	WorkerID          string `json:"workerId"`
	LeaseMilliseconds int64  `json:"leaseMilliseconds"`
}

type proofRequest struct {
	SchemaID string `json:"schemaId"`
	Proof    Proof  `json:"proof"`
}

type advanceRequest struct {
	SchemaID    string  `json:"schemaId"`
	Proof       Proof   `json:"proof"`
	Phase       string  `json:"phase"`
	EffectState string  `json:"effectState"`
	ResultCode  *string `json:"resultCode"`
}

type authorityResponse struct {
	SchemaID string `json:"schemaId"`
	Active   bool   `json:"active"`
}

type Fault struct {
	Status    int
	Code      string
	Retryable bool
}

func (fault *Fault) Error() string { return fault.Code }

type Client struct {
	http        *http.Client
	workerToken string
}

func New(socket string) (*Client, error) {
	return newClient(socket, "")
}

// NewWorker creates the capability-bearing client used only by the separate
// adapter process. Management callers use New and never receive this token.
func NewWorker(socket, workerToken string) (*Client, error) {
	if !validWorkerAccessToken(workerToken) {
		return nil, errors.New("invalid agent-service worker token")
	}
	return newClient(socket, workerToken)
}

func newClient(socket, workerToken string) (*Client, error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || len(socket) > 100 ||
		strings.TrimSpace(socket) != socket || strings.ContainsAny(socket, "\x00\r\n") {
		return nil, errors.New("invalid agent-service socket")
	}
	directoryInfo, err := os.Lstat(filepath.Dir(socket))
	socketInfo, socketErr := os.Lstat(socket)
	directoryUID, directoryOK := fileOwnerUID(directoryInfo)
	socketUID, socketOK := fileOwnerUID(socketInfo)
	if err != nil || socketErr != nil || !directoryOK || !socketOK || !directoryInfo.IsDir() ||
		directoryInfo.Mode()&os.ModeSymlink != 0 || directoryInfo.Mode().Perm() != 0o700 ||
		socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode().Perm() != 0o600 ||
		directoryUID != os.Geteuid() || socketUID != directoryUID {
		return nil, errors.New("unsafe agent-service socket")
	}
	dialer := &net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			currentDirectory, directoryErr := os.Lstat(filepath.Dir(socket))
			currentSocket, socketErr := os.Lstat(socket)
			if directoryErr != nil || socketErr != nil || !os.SameFile(directoryInfo, currentDirectory) ||
				!os.SameFile(socketInfo, currentSocket) {
				return nil, errors.New("agent-service socket identity changed")
			}
			return dialer.DialContext(ctx, "unix", socket)
		},
		ResponseHeaderTimeout: 3 * time.Second, IdleConnTimeout: 30 * time.Second,
		MaxIdleConns: 2, MaxConnsPerHost: 2, DisableCompression: true,
	}
	return &Client{workerToken: workerToken, http: &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}}, nil
}

func fileOwnerUID(info os.FileInfo) (int, bool) {
	if info == nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return int(stat.Uid), ok
}

func (client *Client) Close() {
	if transport, ok := client.http.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

func (client *Client) Target(ctx context.Context, ownerID, nodeID string) (TargetStatus, error) {
	if !actor.MatchString(ownerID) || !uuid.MatchString(nodeID) {
		return TargetStatus{}, invalidFault()
	}
	raw, err := client.call(ctx, ownerID, http.MethodGet, "/internal/v1/operation-targets/"+nodeID, nil, http.StatusOK)
	if err != nil {
		return TargetStatus{}, err
	}
	var target TargetStatus
	if decodeExact(raw, &target) != nil || !validTargetStatus(target) {
		return TargetStatus{}, invalidResponse()
	}
	return target, nil
}

func (client *Client) Accept(ctx context.Context, ownerID string, intent Intent) (Receipt, error) {
	if !actor.MatchString(ownerID) || !validIntent(intent) {
		return Receipt{}, invalidFault()
	}
	raw, err := client.call(ctx, ownerID, http.MethodPost, "/internal/v1/operations", intent, http.StatusAccepted)
	if err != nil {
		return Receipt{}, err
	}
	var receipt Receipt
	if decodeExact(raw, &receipt) != nil || !validReceipt(receipt) || receipt.OperationID != intent.OperationID || receipt.Target != intent.Target {
		return Receipt{}, invalidResponse()
	}
	return receipt, nil
}

func (client *Client) Status(ctx context.Context, ownerID, operationID string) (Status, error) {
	if !actor.MatchString(ownerID) || !actor.MatchString(operationID) {
		return Status{}, invalidFault()
	}
	raw, err := client.call(ctx, ownerID, http.MethodGet, "/internal/v1/operations/"+operationID, nil, http.StatusOK)
	if err != nil {
		return Status{}, err
	}
	var status Status
	if decodeExact(raw, &status) != nil || !validStatus(status) || status.Receipt.OperationID != operationID {
		return Status{}, invalidResponse()
	}
	return status, nil
}

func (client *Client) Claim(ctx context.Context, ownerID, nodeID, workerID string, lease time.Duration) (Work, error) {
	if !actor.MatchString(ownerID) || !uuid.MatchString(nodeID) || !actor.MatchString(workerID) ||
		lease < time.Second || lease > 5*time.Minute || lease%time.Millisecond != 0 || client.workerToken == "" {
		return Work{}, invalidFault()
	}
	request := claimRequest{SchemaID: ClaimRequestSchemaID, NodeID: nodeID, WorkerID: workerID, LeaseMilliseconds: lease.Milliseconds()}
	raw, err := client.call(ctx, ownerID, http.MethodPost, "/internal/v1/operation-workers/claim", request, http.StatusOK)
	if err != nil {
		return Work{}, err
	}
	var work Work
	if decodeExact(raw, &work) != nil || !validWork(work) || work.Proof.NodeID != nodeID || work.Proof.WorkerID != workerID {
		return Work{}, invalidResponse()
	}
	return work, nil
}

func (client *Client) Authority(ctx context.Context, ownerID string, proof Proof) error {
	if !actor.MatchString(ownerID) || !validProof(proof) || client.workerToken == "" {
		return invalidFault()
	}
	raw, err := client.call(ctx, ownerID, http.MethodPost, "/internal/v1/operation-workers/authority",
		proofRequest{SchemaID: AuthoritySchemaID, Proof: proof}, http.StatusOK)
	if err != nil {
		return err
	}
	var response authorityResponse
	if decodeExact(raw, &response) != nil || response.SchemaID != AuthoritySchemaID || !response.Active {
		return invalidResponse()
	}
	return nil
}

func (client *Client) MarkSent(ctx context.Context, ownerID string, proof Proof) (Proof, error) {
	if !actor.MatchString(ownerID) || !validProof(proof) || client.workerToken == "" {
		return Proof{}, invalidFault()
	}
	raw, err := client.call(ctx, ownerID, http.MethodPost, "/internal/v1/operation-workers/sent",
		proofRequest{SchemaID: SentSchemaID, Proof: proof}, http.StatusOK)
	if err != nil {
		return Proof{}, err
	}
	var updated Proof
	if decodeExact(raw, &updated) != nil || !validProof(updated) || updated.OperationID != proof.OperationID ||
		updated.NodeID != proof.NodeID || updated.Generation != proof.Generation || updated.WorkerID != proof.WorkerID ||
		updated.WorkerToken != proof.WorkerToken || updated.OperationVersion < proof.OperationVersion {
		return Proof{}, invalidResponse()
	}
	return updated, nil
}

func (client *Client) Advance(ctx context.Context, ownerID string, proof Proof, phase, effectState string, resultCode *string) (Status, error) {
	if !actor.MatchString(ownerID) || !validProof(proof) || !validPhase(phase) || !validEffect(effectState) ||
		(resultCode != nil && !actor.MatchString(*resultCode)) || client.workerToken == "" {
		return Status{}, invalidFault()
	}
	raw, err := client.call(ctx, ownerID, http.MethodPost, "/internal/v1/operation-workers/advance", advanceRequest{
		SchemaID: AdvanceSchemaID, Proof: proof, Phase: phase, EffectState: effectState, ResultCode: resultCode,
	}, http.StatusOK)
	if err != nil {
		return Status{}, err
	}
	var status Status
	if decodeExact(raw, &status) != nil || !validStatus(status) || status.Receipt.OperationID != proof.OperationID {
		return Status{}, invalidResponse()
	}
	return status, nil
}

func (client *Client) call(ctx context.Context, ownerID, method, path string, value any, success int) ([]byte, error) {
	var body io.Reader
	if value != nil {
		raw, err := json.Marshal(value)
		if err != nil || len(raw) > 64<<10 {
			return nil, invalidFault()
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://agent-service"+path, body)
	if err != nil {
		return nil, unavailableFault()
	}
	request.Header.Set(OwnerHeader, ownerID)
	if strings.HasPrefix(path, "/internal/v1/operation-workers/") {
		if client.workerToken == "" {
			return nil, invalidFault()
		}
		request.Header.Set(WorkerTokenHeader, client.workerToken)
	}
	request.Header.Set("Accept", "application/json")
	if value != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.http.Do(request)
	if err != nil {
		return nil, unavailableFault()
	}
	defer response.Body.Close()
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, maximumOperationBody+1))
	if mediaErr != nil || mediaType != "application/json" || response.Header.Get("Content-Encoding") != "" ||
		readErr != nil || len(raw) > maximumOperationBody || !strictjson.Valid(raw) {
		return nil, invalidResponse()
	}
	if response.StatusCode == success {
		return raw, nil
	}
	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if decodeExact(raw, &envelope) != nil || !safeFaultCode(envelope.Error.Code) {
		return nil, invalidResponse()
	}
	status := response.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusServiceUnavailable
	}
	return nil, &Fault{Status: status, Code: envelope.Error.Code, Retryable: envelope.Error.Retryable}
}

func decodeExact(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid response")
	}
	return nil
}

func validTarget(value Target) bool {
	return uuid.MatchString(value.NodeID) && uuid.MatchString(value.HostID) &&
		value.RegistrationRevision >= 1 && value.RegistrationRevision <= maximumSafeInteger &&
		value.RegistrationEpoch >= 1 && value.RegistrationEpoch <= maximumSafeInteger &&
		value.Generation >= 0 && value.Generation < maximumSafeInteger
}

func validTargetStatus(value TargetStatus) bool {
	return value.SchemaID == TargetSchemaID && validTarget(value.Target) &&
		(value.RegistryMode == "fixture" || value.RegistryMode == "live") &&
		(value.RegistrationMode == "compatible" || value.RegistrationMode == "legacy_readonly")
}

func validIntent(value Intent) bool {
	if value.SchemaID != IntentSchemaID || !actor.MatchString(value.OperationID) || value.Kind != "adapter.fixture" ||
		!validTarget(value.Target) || !actor.MatchString(value.Step.StepID) || value.Step.Action != "adapter.fixture.apply" ||
		len(value.Step.ResourceIDs) == 0 || len(value.Step.ResourceIDs) > 32 || !slices.IsSorted(value.Step.ResourceIDs) {
		return false
	}
	for index, resourceID := range value.Step.ResourceIDs {
		if !actor.MatchString(resourceID) || index > 0 && resourceID == value.Step.ResourceIDs[index-1] {
			return false
		}
	}
	return true
}

func validProof(value Proof) bool {
	if value.SchemaID != ProofSchemaID || !actor.MatchString(value.OperationID) || !hash.MatchString(value.RequestHash) ||
		!uuid.MatchString(value.NodeID) ||
		value.Generation < 1 || value.Generation > maximumSafeInteger || !actor.MatchString(value.WorkerID) ||
		!uuid.MatchString(value.WorkerToken) || value.OperationVersion < 1 || value.OperationVersion > maximumSafeInteger {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.LeaseExpiresAt)
	return err == nil && value.LeaseExpiresAt == parsed.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
}

func validWork(value Work) bool {
	requestHash, err := intentRequestHash(value.Intent)
	return value.SchemaID == WorkSchemaID && err == nil && validProof(value.Proof) && validEffect(value.EffectState) &&
		value.Proof.OperationID == value.Intent.OperationID && value.Proof.NodeID == value.Intent.Target.NodeID &&
		value.Proof.Generation == value.Intent.Target.Generation+1 && value.Proof.RequestHash == requestHash
}

func intentRequestHash(value Intent) (string, error) {
	if !validIntent(value) {
		return "", errors.New("invalid operation intent")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", errors.New("cannot encode operation intent")
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func validReceipt(value Receipt) bool {
	if value.SchemaID != ReceiptSchemaID || !actor.MatchString(value.OperationID) || !hash.MatchString(value.RequestHash) ||
		!validTarget(value.Target) || value.AcceptedGeneration != value.Target.Generation+1 {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.AcceptedAt)
	return err == nil && value.AcceptedAt == parsed.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
}

func validStatus(value Status) bool {
	if value.SchemaID != StatusSchemaID || !validReceipt(value.Receipt) || !validPhase(value.Phase) ||
		!validEffect(value.EffectState) || value.OperationVersion < 1 || value.OperationVersion > maximumSafeInteger ||
		(value.ResultCode != nil && !actor.MatchString(*value.ResultCode)) {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.UpdatedAt)
	if err != nil || value.UpdatedAt != parsed.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano) {
		return false
	}
	return value.EffectState != "unknown" || value.Phase == "reconciling"
}

func validPhase(value string) bool {
	return slices.Contains([]string{"accepted", "validating", "waiting", "building", "preparing", "applying", "verifying", "reconciling", "succeeded", "failed"}, value)
}

func validEffect(value string) bool {
	return slices.Contains([]string{"not_sent", "sent", "acknowledged", "reconciled", "unknown", "failed"}, value)
}

func safeFaultCode(value string) bool {
	return slices.Contains([]string{"invalid_request", "owner_scope_required", "worker_scope_required", "not_found", "operation_conflict", "stale_worker", "operation_unavailable"}, value)
}

func validWorkerAccessToken(value string) bool {
	if len(value) < 32 || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("._:@-", character) {
			continue
		}
		return false
	}
	return true
}

func invalidFault() error { return &Fault{Status: http.StatusBadRequest, Code: "invalid_request"} }
func unavailableFault() error {
	return &Fault{Status: http.StatusServiceUnavailable, Code: "operation_unavailable", Retryable: true}
}
func invalidResponse() error {
	return &Fault{Status: http.StatusServiceUnavailable, Code: "invalid_operation_response", Retryable: true}
}

// RemoteAuthority binds one claimed operation to its exact host/node and turns
// the executor's sent barrier into a DB operation-step transition.
type RemoteAuthority struct {
	mu      sync.Mutex
	client  *Client
	owner   string
	target  Target
	request dockeradapter.Request
	proof   Proof
}

func NewRemoteAuthority(client *Client, ownerID string, work Work) (*RemoteAuthority, error) {
	request, err := AdapterRequest(work)
	if client == nil || !actor.MatchString(ownerID) || err != nil {
		return nil, errors.New("invalid remote operation authority")
	}
	return &RemoteAuthority{
		client: client, owner: ownerID, target: work.Intent.Target,
		request: request, proof: work.Proof,
	}, nil
}

// AdapterRequest derives the only R02 effect request from the exact DB-backed
// work. The proof request hash covers the complete intent, including its step
// ID, action, target and ordered resource IDs.
func AdapterRequest(work Work) (dockeradapter.Request, error) {
	if !validWork(work) {
		return dockeradapter.Request{}, errors.New("invalid remote operation work")
	}
	return dockeradapter.Request{
		SchemaID: dockeradapter.RequestSchemaID, OperationID: work.Intent.OperationID,
		StepID: work.Intent.Step.StepID, Generation: work.Proof.Generation,
		Action: "fixture.apply", ResourceIDs: append([]string(nil), work.Intent.Step.ResourceIDs...),
	}, nil
}

func (authority *RemoteAuthority) VerifyActive(ctx context.Context, daemonID, instanceID string, request dockeradapter.Request, proof dockeradapter.AuthorityProof) error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if !authority.matches(daemonID, instanceID, request, proof) || authority.client.Authority(ctx, authority.owner, authority.proof) != nil {
		return dockeradapter.ErrStaleAuthority
	}
	return nil
}

func (authority *RemoteAuthority) RecordSent(ctx context.Context, daemonID, instanceID string, request dockeradapter.Request, proof dockeradapter.AuthorityProof) (dockeradapter.AuthorityProof, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if !authority.matches(daemonID, instanceID, request, proof) {
		return dockeradapter.AuthorityProof{}, dockeradapter.ErrStaleAuthority
	}
	updated, err := authority.client.MarkSent(ctx, authority.owner, authority.proof)
	if err != nil {
		return dockeradapter.AuthorityProof{}, dockeradapter.ErrStaleAuthority
	}
	authority.proof = updated
	return adapterProof(updated), nil
}

func (authority *RemoteAuthority) Proof() Proof {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.proof
}

func (authority *RemoteAuthority) matches(daemonID, instanceID string, request dockeradapter.Request, proof dockeradapter.AuthorityProof) bool {
	return daemonID == authority.target.HostID && instanceID == authority.target.NodeID &&
		request.SchemaID == authority.request.SchemaID && request.OperationID == authority.request.OperationID &&
		request.StepID == authority.request.StepID && request.Generation == authority.request.Generation &&
		request.Action == authority.request.Action && slices.Equal(request.ResourceIDs, authority.request.ResourceIDs) &&
		proof == adapterProof(authority.proof)
}

func AdapterProof(value Proof) dockeradapter.AuthorityProof { return adapterProof(value) }

func adapterProof(value Proof) dockeradapter.AuthorityProof {
	return dockeradapter.AuthorityProof{
		OperationID: value.OperationID, Generation: value.Generation, WorkerID: value.WorkerID,
		WorkerToken: value.WorkerToken, OperationVersion: value.OperationVersion,
	}
}
