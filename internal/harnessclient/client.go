package harnessclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessbarrier"
	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
	"github.com/boxvtk621/homelab-telegram-panel/internal/transcriptview"
)

// Fault contains only a fixed wire code; transport errors and response bodies
// are never exposed as operational log messages.
type Fault struct {
	Status int
	Code   string
}

func (f *Fault) Error() string { return f.Code }
func invalid() error           { return &Fault{Status: 400, Code: "invalid"} }
func unavailable() error       { return &Fault{Status: 503, Code: "node_unavailable"} }
func mismatch() error          { return &Fault{Status: 409, Code: "schema_mismatch"} }
func stale() error             { return &Fault{Status: 409, Code: "stale"} }

type Response struct {
	Status int
	Body   []byte
}

type readRoute struct {
	path, wireType      string
	scopeField, scopeID string
	dialogID, attemptID string
	source              *transcriptview.Source
}

// ParseRead permits known node routes and exact query keys only. Arbitrary
// browser paths never become private requests, even with a valid owner session.
func ParseRead(path, rawQuery string) (readRoute, error) {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return readRoute{}, invalid()
	}
	r := readRoute{path: path}
	allowed := map[string]bool{}
	parts := strings.Split(path, "/")
	switch path {
	case "identity":
		r.wireType = "nodeIdentity"
	case "snapshot":
		r.wireType = "snapshot"
	case "health/ready":
		r.wireType = "healthReady"
	case "health/live":
		r.wireType = "healthLive"
	case "dialogs":
		r.wireType = "dialogPage"
		allowed["cursor"], allowed["limit"] = true, true
	case "requests":
		r.wireType = "requestPage"
		allowed["cursor"], allowed["limit"], allowed["state"] = true, true, true
	case "texts/resolve":
		r.wireType = "transcriptManifest"
		for _, key := range []string{"dialogId", "attemptId", "sourceKind", "sourceId", "sourceIndex", "sourceStream"} {
			allowed[key] = true
		}
	default:
		if len(parts) < 2 || !uuid.MatchString(parts[1]) {
			return readRoute{}, invalid()
		}
		switch {
		case len(parts) == 3 && parts[0] == "dialogs" && parts[2] == "messages":
			r.wireType, r.scopeField, r.scopeID = "historyPage", "dialogId", parts[1]
			allowed["cursor"], allowed["limit"] = true, true
		case len(parts) == 3 && parts[0] == "requests" && parts[2] == "attempts":
			r.wireType, r.scopeField, r.scopeID = "attemptPage", "requestId", parts[1]
			allowed["cursor"], allowed["limit"] = true, true
		case len(parts) == 3 && parts[0] == "attempts" && parts[2] == "events":
			r.wireType, r.scopeField, r.scopeID = "eventPage", "attemptId", parts[1]
			allowed["after"], allowed["limit"] = true, true
		case len(parts) == 3 && parts[0] == "artifacts" && parts[2] == "metadata":
			r.wireType, r.scopeField, r.scopeID = "artifactMetadata", "artifactId", parts[1]
		case len(parts) == 2 && parts[0] == "attempts":
			r.wireType, r.scopeField, r.scopeID = "attemptRead", "attemptId", parts[1]
		case len(parts) == 2 && parts[0] == "commands":
			r.wireType, r.scopeField, r.scopeID = "commandStatus", "commandId", parts[1]
		default:
			return readRoute{}, invalid()
		}
	}
	for k, values := range q {
		if !allowed[k] || len(values) != 1 || values[0] == "" {
			return readRoute{}, invalid()
		}
		v := values[0]
		switch k {
		case "cursor":
			if len(v) > hp.MaximumCursorBytes || strings.ContainsAny(v, "\r\n\x00") {
				return readRoute{}, invalid()
			}
		case "limit":
			if n, ok := safeNumber(v); !ok || n < 1 || n > hp.MaximumPageSize {
				return readRoute{}, invalid()
			}
		case "after":
			if _, ok := safeNumber(v); !ok {
				return readRoute{}, invalid()
			}
		case "state":
			if !strings.Contains("|queued|cancelled|dispatching|active|completed|failed|interrupted|unknown|", "|"+v+"|") || strings.Contains(v, "|") {
				return readRoute{}, invalid()
			}
		case "dialogId", "attemptId", "sourceId":
			if !uuid.MatchString(v) {
				return readRoute{}, invalid()
			}
		case "sourceIndex":
			if _, ok := safeNumber(v); !ok {
				return readRoute{}, invalid()
			}
		case "sourceKind", "sourceStream":
			// Relational validation is performed below by transcript-view-v1.
		}
	}
	if r.wireType == "transcriptManifest" {
		if len(q) != 6 {
			return readRoute{}, invalid()
		}
		index, ok := safeNumber(q.Get("sourceIndex"))
		if !ok {
			return readRoute{}, invalid()
		}
		source := transcriptview.Source{Kind: q.Get("sourceKind"), ID: q.Get("sourceId"), Index: index, Stream: q.Get("sourceStream")}
		if !uuid.MatchString(q.Get("dialogId")) || !uuid.MatchString(q.Get("attemptId")) || transcriptview.ValidateSource(source) != nil {
			return readRoute{}, invalid()
		}
		r.dialogID, r.attemptID, r.source = q.Get("dialogId"), q.Get("attemptId"), &source
	}
	if len(q) > 0 {
		r.path += "?" + q.Encode()
	}
	return r, nil
}

func safeNumber(value string) (int64, bool) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	return n, err == nil && n <= hp.MaximumSafeInteger
}

func (e *entry) request(ctx context.Context, owner, method, path string, body []byte) (*http.Response, error) {
	return e.requestAcceptExpected(ctx, owner, method, path, body, "application/json", nil)
}

func (e *entry) requestAccept(ctx context.Context, owner, method, path string, body []byte, accept string) (*http.Response, error) {
	return e.requestAcceptExpected(ctx, owner, method, path, body, accept, nil)
}

func (e *entry) requestAcceptExpected(ctx context.Context, owner, method, path string, body []byte, accept string, expected *hp.NodeIdentity) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, e.node.URL+path, bytes.NewReader(body))
	if err != nil {
		return nil, invalid()
	}
	req.Header.Set("X-Harness-Actor-ID", owner)
	req.Header.Set("Accept", accept)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if expected != nil {
		req.Header.Set(hp.ExpectedNodeIDHeader, expected.NodeID)
		req.Header.Set(hp.ExpectedRegistryHeader, strconv.FormatInt(expected.RegistryVersion, 10))
		req.Header.Set(hp.ExpectedEpochHeader, strconv.FormatInt(expected.IdentityEpoch, 10))
		req.Header.Set(hp.ExpectedAdapterKindHeader, expected.Adapter.Kind)
		req.Header.Set(hp.ExpectedAdapterVersionHeader, expected.Adapter.Version)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, unavailable()
	}
	return resp, nil
}

func jsonBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || resp.Header.Get("Content-Encoding") != "" {
		return nil, unavailable()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, hp.MaximumWireBytes+1))
	if err != nil {
		return nil, unavailable()
	}
	if len(body) > hp.MaximumWireBytes {
		return nil, unavailable()
	}
	return body, nil
}

func (c *Client) handshake(ctx context.Context, e *entry, owner string) (hp.NodeIdentity, []byte, error) {
	resp, err := e.request(ctx, owner, http.MethodGet, "/v1/nodes/"+e.node.NodeID+"/identity", nil)
	if err != nil {
		return hp.NodeIdentity{}, nil, err
	}
	body, err := jsonBody(resp)
	if err != nil {
		return hp.NodeIdentity{}, nil, err
	}
	if resp.StatusCode != 200 {
		return hp.NodeIdentity{}, nil, unavailable()
	}
	if hp.Validate("nodeIdentity", body) != nil {
		return hp.NodeIdentity{}, nil, mismatch()
	}
	var id hp.NodeIdentity
	if json.Unmarshal(body, &id) != nil || id.NodeID != e.node.NodeID || id.RegistryVersion != c.registryVersion(e.node) || id.Adapter.Kind != e.node.Adapter {
		return hp.NodeIdentity{}, nil, mismatch()
	}
	return id, body, nil
}

func (c *Client) Read(ctx context.Context, nodeID, owner, path, query string) (Response, error) {
	route, err := ParseRead(path, query)
	if err != nil {
		return Response{}, err
	}
	e, err := c.node(nodeID, owner)
	if err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	identity, body, err := c.handshake(ctx, e, owner)
	if err != nil {
		return Response{}, err
	}
	if route.wireType == "nodeIdentity" {
		return Response{Status: 200, Body: body}, nil
	}
	upstream := "/v1/nodes/" + nodeID + "/" + route.path
	if strings.HasPrefix(route.path, "health/") {
		upstream = "/" + route.path
	}
	resp, err := e.request(ctx, owner, http.MethodGet, upstream, nil)
	if err != nil {
		return Response{}, err
	}
	body, err = jsonBody(resp)
	if err != nil {
		return Response{}, err
	}
	if resp.StatusCode != 200 {
		if route.wireType == "commandStatus" && validBarrierRejection(resp.StatusCode, body, identity.NodeID, route.scopeID, identity.IdentityEpoch, "", "") {
			return Response{Status: resp.StatusCode, Body: body}, nil
		}
		if err := validateErrorStatus(resp.StatusCode, body); err != nil {
			return Response{}, err
		}
		return Response{Status: resp.StatusCode, Body: body}, nil
	}
	valid := false
	if route.wireType == "transcriptManifest" {
		_, err := transcriptview.Decode(body)
		valid = err == nil
	} else {
		valid = hp.Validate(route.wireType, body) == nil
	}
	if !valid || !matchesReadScope(route, identity, body) {
		return Response{}, mismatch()
	}
	return Response{Status: 200, Body: body}, nil
}

func matchesReadScope(route readRoute, id hp.NodeIdentity, body []byte) bool {
	if route.wireType == "transcriptManifest" {
		manifest, err := transcriptview.Decode(body)
		return err == nil && route.source != nil && manifest.NodeID == id.NodeID &&
			manifest.DialogID == route.dialogID && manifest.AttemptID == route.attemptID && manifest.Source == *route.source
	}
	var v map[string]json.RawMessage
	if json.Unmarshal(body, &v) != nil {
		return false
	}
	if route.wireType == "healthLive" {
		return true
	}
	if route.wireType == "healthReady" {
		var h hp.HealthReady
		if json.Unmarshal(body, &h) != nil {
			return false
		}
		return h.Identity.NodeID == id.NodeID && h.Identity.RegistryVersion == id.RegistryVersion && h.Identity.IdentityEpoch == id.IdentityEpoch && h.Identity.Adapter == id.Adapter
	}
	var node string
	var epoch int64
	if json.Unmarshal(v["nodeId"], &node) != nil || node != id.NodeID {
		return false
	}
	if raw, ok := v["epoch"]; ok {
		if json.Unmarshal(raw, &epoch) != nil || epoch != id.IdentityEpoch {
			return false
		}
	}
	if route.scopeField != "" {
		if route.wireType == "attemptRead" {
			if json.Unmarshal(v["attempt"], &v) != nil {
				return false
			}
		}
		var scope string
		if json.Unmarshal(v[route.scopeField], &scope) != nil || scope != route.scopeID {
			return false
		}
	}
	return true
}

// Command performs exactly one POST after the identity handshake. It never
// retries, generates a command ID, or manufactures a receipt after lost ACK.
func (c *Client) command(ctx context.Context, nodeID, owner string, body []byte, expected *hp.NodeIdentity) (Response, error) {
	_, canonicalPayloadHash, canonicalErr := hp.CanonicalCommand(body)
	if canonicalErr != nil {
		return Response{}, invalid()
	}
	var cmd hp.CommandEnvelope
	var target map[string]string
	if json.Unmarshal(body, &cmd) != nil || json.Unmarshal(cmd.Target, &target) != nil || target["nodeId"] != nodeID {
		return Response{}, invalid()
	}
	e, err := c.node(nodeID, owner)
	if err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	identity, _, err := c.handshake(ctx, e, owner)
	if err != nil {
		return Response{}, err
	}
	if expected != nil && (identity.NodeID != expected.NodeID || identity.RegistryVersion != expected.RegistryVersion ||
		identity.IdentityEpoch != expected.IdentityEpoch || identity.Adapter != expected.Adapter) {
		return Response{}, stale()
	}
	admissionIdentity := identity
	if expected != nil {
		admissionIdentity = *expected
	}
	resp, err := e.requestAcceptExpected(ctx, owner, http.MethodPost, "/v1/nodes/"+nodeID+"/commands", body, "application/json", &admissionIdentity)
	if err != nil {
		return Response{}, err
	}
	result, err := jsonBody(resp)
	if err != nil {
		return Response{}, unavailable()
	}
	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		if validBarrierRejection(resp.StatusCode, result, nodeID, cmd.CommandID, admissionIdentity.IdentityEpoch, cmd.Kind, canonicalPayloadHash) {
			return Response{Status: resp.StatusCode, Body: result}, nil
		}
		if err := validateErrorStatus(resp.StatusCode, result); err != nil {
			return Response{}, err
		}
		return Response{Status: resp.StatusCode, Body: result}, nil
	}
	if hp.Validate("receipt", result) != nil {
		return Response{}, unavailable()
	}
	var receipt hp.Receipt
	var refs map[string]string
	if json.Unmarshal(result, &receipt) != nil || json.Unmarshal(receipt.References, &refs) != nil || receipt.CommandID != cmd.CommandID || receipt.CommandKind != cmd.Kind || receipt.NodeID != nodeID {
		return Response{}, unavailable()
	}
	for key, want := range target {
		refKey := key
		if cmd.Kind == hp.CommandAttemptRetry && key == "attemptId" {
			refKey = "priorAttemptId"
		}
		if got, present := refs[refKey]; present && got != want {
			return Response{}, unavailable()
		}
	}
	return Response{Status: resp.StatusCode, Body: result}, nil
}

// Command performs one command without an external admission fence. Harness
// integration clients use this directly; the managed Router uses CommandFenced.
func (c *Client) Command(ctx context.Context, nodeID, owner string, body []byte) (Response, error) {
	return c.command(ctx, nodeID, owner, body, nil)
}

// CommandFenced compares the live handshake with the durable Router identity
// before the single POST. A restarted or replaced node therefore cannot accept
// work until an operator has explicitly activated that exact identity.
func (c *Client) CommandFenced(ctx context.Context, nodeID, owner string, body []byte, expected hp.NodeIdentity) (Response, error) {
	return c.command(ctx, nodeID, owner, body, &expected)
}

// InstallHold sends one exact administrative hold request after binding it to
// a fresh authenticated identity. Administrative calls are never retried.
func (c *Client) InstallHold(ctx context.Context, nodeID, owner string, body []byte) (Response, error) {
	if harnessbarrier.Validate("installRequest", body) != nil {
		return Response{}, invalid()
	}
	var request harnessbarrier.InstallRequest
	if json.Unmarshal(body, &request) != nil || request.NodeID != nodeID {
		return Response{}, invalid()
	}
	return c.administrative(ctx, nodeID, owner, http.MethodPost, "/v1/nodes/"+nodeID+"/administration/holds", body,
		"holdReceipt", request.OperationID, request.ExpectedEpoch, request.BindingGeneration, request.Scope, request.ExpectedScopeRevision, 0)
}

// QuiescenceProof performs the mandatory current readback. A cached proof is
// never manufactured or accepted when the authenticated identity changed.
func (c *Client) QuiescenceProof(ctx context.Context, nodeID, owner, operationID string) (Response, error) {
	if !actor.MatchString(operationID) {
		return Response{}, invalid()
	}
	return c.administrative(ctx, nodeID, owner, http.MethodGet,
		"/v1/nodes/"+nodeID+"/administration/holds/"+url.PathEscape(operationID)+"/proof", nil,
		"quiescenceProof", operationID, 0, 0, harnessbarrier.Scope{}, 0, 0)
}

// ReleaseHold durably releases or cancels only the hold named in the exact
// request. Lost acknowledgements are resolved by repeating the same bytes.
func (c *Client) ReleaseHold(ctx context.Context, nodeID, owner string, body []byte) (Response, error) {
	if harnessbarrier.Validate("releaseRequest", body) != nil {
		return Response{}, invalid()
	}
	var request harnessbarrier.ReleaseRequest
	if json.Unmarshal(body, &request) != nil || request.NodeID != nodeID {
		return Response{}, invalid()
	}
	return c.administrative(ctx, nodeID, owner, http.MethodPost,
		"/v1/nodes/"+nodeID+"/administration/holds/"+url.PathEscape(request.OperationID)+"/release", body,
		"releaseReceipt", request.OperationID, request.ExpectedEpoch, request.BindingGeneration, request.Scope, request.ExpectedScopeRevision, request.HoldVersion)
}

func (c *Client) administrative(ctx context.Context, nodeID, owner, method, path string, body []byte, wireType, operationID string,
	expectedEpoch, expectedBinding int64, expectedScope harnessbarrier.Scope, expectedScopeRevision, expectedHoldVersion int64) (Response, error) {
	e, err := c.node(nodeID, owner)
	if err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	identity, _, err := c.handshake(ctx, e, owner)
	if err != nil {
		return Response{}, err
	}
	if expectedEpoch != 0 && (expectedEpoch != identity.IdentityEpoch || expectedBinding != identity.RegistryVersion) {
		return Response{}, stale()
	}
	response, err := e.request(ctx, owner, method, path, body)
	if err != nil {
		return Response{}, err
	}
	result, err := jsonBody(response)
	if err != nil {
		return Response{}, unavailable()
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		if err := validateErrorStatus(response.StatusCode, result); err != nil {
			return Response{}, err
		}
		return Response{Status: response.StatusCode, Body: result}, nil
	}
	if harnessbarrier.Validate(wireType, result) != nil {
		return Response{}, mismatch()
	}
	switch wireType {
	case "holdReceipt":
		var receipt harnessbarrier.HoldReceipt
		if json.Unmarshal(result, &receipt) != nil || receipt.OperationID != operationID || receipt.NodeID != nodeID ||
			receipt.Epoch != identity.IdentityEpoch || receipt.BindingGeneration != identity.RegistryVersion ||
			receipt.Scope != expectedScope || receipt.ScopeRevision != expectedScopeRevision+1 {
			return Response{}, mismatch()
		}
	case "quiescenceProof":
		var proof harnessbarrier.QuiescenceProof
		if json.Unmarshal(result, &proof) != nil || proof.OperationID != operationID || proof.NodeID != nodeID ||
			proof.Epoch != identity.IdentityEpoch || proof.BindingGeneration != identity.RegistryVersion {
			return Response{}, mismatch()
		}
	case "releaseReceipt":
		var receipt harnessbarrier.ReleaseReceipt
		if json.Unmarshal(result, &receipt) != nil || receipt.OperationID != operationID || receipt.NodeID != nodeID ||
			receipt.Epoch != identity.IdentityEpoch || receipt.BindingGeneration != identity.RegistryVersion ||
			receipt.Scope != expectedScope || receipt.HoldVersion != expectedHoldVersion || receipt.ScopeRevision != expectedScopeRevision+1 {
			return Response{}, mismatch()
		}
	}
	return Response{Status: response.StatusCode, Body: result}, nil
}

func validBarrierRejection(status int, body []byte, nodeID, commandID string, epoch int64, commandKind hp.CommandKind, canonicalPayloadHash string) bool {
	if status != http.StatusConflict || harnessbarrier.Validate("rejectionReceipt", body) != nil {
		return false
	}
	var receipt harnessbarrier.RejectionReceipt
	if json.Unmarshal(body, &receipt) != nil || receipt.NodeID != nodeID || receipt.CommandID != commandID || receipt.Epoch != epoch {
		return false
	}
	if commandKind != "" && receipt.CommandKind != string(commandKind) {
		return false
	}
	return canonicalPayloadHash == "" || receipt.CanonicalPayloadHash == canonicalPayloadHash
}

func validateErrorStatus(status int, body []byte) error {
	if hp.Validate("error", body) != nil {
		return unavailable()
	}
	var e hp.Error
	if json.Unmarshal(body, &e) != nil {
		return unavailable()
	}
	allowed := map[string]int{"invalid": 400, "no_session": 401, "forbidden": 403, "not_found": 404, "stale": 409, "id_conflict": 409, "too_large": 413, "queue_full": 429, "node_unavailable": 503, "not_durable": 503, "unsupported": 422, "protocol_mismatch": 409, "schema_mismatch": 409}
	if want, ok := allowed[e.Code]; !ok || status != want {
		return unavailable()
	}
	return nil
}
