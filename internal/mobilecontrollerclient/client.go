// Package mobilecontrollerclient is the only Mobile Gateway path to the
// authoritative Controller. It speaks the reviewed HTTP/JSON protocol over
// four local Unix sockets and never opens TCP or reads Controller storage.
package mobilecontrollerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	internalPrefix       = "/internal/mobile/v2"
	protocolSchema       = 2
	maximumResponseBytes = 1024 * 1024
	defaultTimeout       = 5 * time.Second
)

// Dialog is the exact safe Controller projection accepted by the Gateway.
type Dialog struct {
	ID                   string `json:"id"`
	ActiveTaskID         string `json:"active_task_id,omitempty"`
	PendingAdmissionID   string `json:"pending_admission_id,omitempty"`
	AdmissionNextCheckAt string `json:"admission_next_check_at,omitempty"`
	Lifecycle            string `json:"lifecycle"`
	ObjectVersion        int64  `json:"object_version"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
}

type DialogList struct {
	Dialogs         []Dialog `json:"dialogs"`
	SnapshotVersion int64    `json:"snapshot_version"`
	FreshnessAt     string   `json:"freshness_at"`
	NextCursor      string   `json:"next_cursor,omitempty"`
	EventCheckpoint string   `json:"event_checkpoint,omitempty"`
}

type DialogSnapshot struct {
	Dialog          Dialog `json:"dialog"`
	SnapshotVersion int64  `json:"snapshot_version"`
	FreshnessAt     string `json:"freshness_at"`
}

type CreateDialogRequest struct {
	CommandID                 string `json:"command_id"`
	ClientInstanceID          string `json:"client_instance_id"`
	ExpectedCollectionVersion int64  `json:"expected_collection_version"`
}

type CreateDialogResult struct {
	CommandID       string `json:"command_id"`
	Replayed        bool   `json:"replayed"`
	Dialog          Dialog `json:"dialog"`
	SnapshotVersion int64  `json:"snapshot_version"`
	FreshnessAt     string `json:"freshness_at"`
}

type ManagementEvent struct {
	EventID       int64  `json:"event_id"`
	OwnerVersion  int64  `json:"owner_version"`
	DialogID      string `json:"dialog_id"`
	ObjectVersion int64  `json:"object_version"`
	Kind          string `json:"kind"`
	Lifecycle     string `json:"lifecycle"`
	OccurredAt    string `json:"occurred_at"`
}

type EventList struct {
	Events          []ManagementEvent `json:"events"`
	SnapshotVersion int64             `json:"snapshot_version"`
	FreshnessAt     string            `json:"freshness_at"`
	NextCursor      string            `json:"next_cursor,omitempty"`
	HasMore         bool              `json:"has_more"`
}

type Message struct {
	ID           string `json:"id"`
	DialogID     string `json:"dialog_id"`
	Sequence     int64  `json:"sequence"`
	Actor        string `json:"actor"`
	Kind         string `json:"kind"`
	Content      string `json:"content"`
	SupersedesID string `json:"supersedes_id,omitempty"`
	CreatedAt    string `json:"created_at"`
}

type MessageList struct {
	Messages          []Message `json:"messages"`
	HasMore           bool      `json:"has_more"`
	NextAfterSequence *int64    `json:"next_after_sequence,omitempty"`
	FreshnessAt       string    `json:"freshness_at"`
}

type Task struct {
	ID               string `json:"id"`
	DialogID         string `json:"dialog_id"`
	IssueID          string `json:"issue_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	State            string `json:"state"`
	CancelState      string `json:"cancel_state"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	NextCheckAt      string `json:"next_check_at,omitempty"`
}

type TaskList struct {
	Tasks       []Task `json:"tasks"`
	FreshnessAt string `json:"freshness_at"`
}

type TaskSnapshot struct {
	Task        Task   `json:"task"`
	FreshnessAt string `json:"freshness_at"`
}

type ControlComponent struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

type ControlCapacity struct {
	Class    string `json:"class"`
	InFlight int    `json:"in_flight"`
	Limit    int    `json:"limit"`
}

type ControlSummary struct {
	Components  []ControlComponent `json:"components"`
	Capacity    []ControlCapacity  `json:"capacity"`
	FreshnessAt string             `json:"freshness_at"`
}

type Status struct {
	Status string `json:"status"`
}

// Identity is the non-secret Controller authority binding returned only on the
// authenticated private health socket. It is never derived from browser input.
type Identity struct {
	Principal           string `json:"principal"`
	CapabilitiesVersion string `json:"capabilities_version"`
}

// RemoteError contains only the Controller's bounded code and HTTP status.
// The remote message is deliberately discarded.
type RemoteError struct {
	Status int
	Code   string
}

func (err *RemoteError) Error() string { return "Controller request rejected" }

type endpoint struct {
	http       *http.Client
	socketPath string
}

// Client has physically separate general, health, status, and recovery
// transports. No pool borrows connections from another class.
type Client struct {
	business             endpoint
	health               endpoint
	control              endpoint
	recovery             endpoint
	expectedPrincipal    string
	expectedCapabilities string
}

// New constructs bounded no-proxy clients for four distinct absolute Unix
// socket paths. Control is the status class retained under its protocol name.
func New(
	businessSocket, healthSocket, controlSocket, recoverySocket string,
	expectedPrincipal, expectedCapabilities string,
) (*Client, error) {
	if !safePathSegment(expectedPrincipal) || !safePathSegment(expectedCapabilities) {
		return nil, errors.New("Controller authority binding is invalid")
	}
	paths := []string{businessSocket, healthSocket, controlSocket, recoverySocket}
	for left := range paths {
		for right := left + 1; right < len(paths); right++ {
			if paths[left] == paths[right] {
				return nil, errors.New("Controller Unix socket paths must be distinct")
			}
		}
	}
	business, err := newEndpoint(businessSocket, defaultTimeout, 24)
	if err != nil {
		return nil, err
	}
	health, err := newEndpoint(healthSocket, 2*time.Second, 2)
	if err != nil {
		business.http.CloseIdleConnections()
		return nil, err
	}
	control, err := newEndpoint(controlSocket, 2*time.Second, 2)
	if err != nil {
		business.http.CloseIdleConnections()
		health.http.CloseIdleConnections()
		return nil, err
	}
	recovery, err := newEndpoint(recoverySocket, defaultTimeout, 2)
	if err != nil {
		business.http.CloseIdleConnections()
		health.http.CloseIdleConnections()
		control.http.CloseIdleConnections()
		return nil, err
	}
	return &Client{
		business: business, health: health, control: control, recovery: recovery,
		expectedPrincipal: expectedPrincipal, expectedCapabilities: expectedCapabilities,
	}, nil
}

func newEndpoint(socketPath string, timeout time.Duration, maximumConnections int) (endpoint, error) {
	if socketPath == "" || !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath ||
		strings.ContainsRune(socketPath, '\x00') || len(socketPath) > 100 {
		return endpoint{}, errors.New("Controller Unix socket path is invalid")
	}
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          maximumConnections,
		MaxIdleConnsPerHost:   maximumConnections,
		MaxConnsPerHost:       maximumConnections,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: timeout,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return endpoint{http: client, socketPath: socketPath}, nil
}

// CloseIdleConnections releases all four isolated connection pools.
func (client *Client) CloseIdleConnections() {
	if client == nil {
		return
	}
	client.business.http.CloseIdleConnections()
	client.health.http.CloseIdleConnections()
	client.control.http.CloseIdleConnections()
	client.recovery.http.CloseIdleConnections()
}

func (client *Client) ListDialogs(ctx context.Context, requestID string, limit int, cursor string) (DialogList, error) {
	return client.listDialogs(ctx, client.business, internalPrefix+"/dialogs", requestID, limit, cursor)
}

// RecoverDialogs uses the isolated recovery transport and explicit private
// endpoint. Ordinary Dialog pagination must use ListDialogs instead.
func (client *Client) RecoverDialogs(ctx context.Context, requestID string, limit int, cursor string) (DialogList, error) {
	return client.listDialogs(ctx, client.recovery, internalPrefix+"/recovery/dialogs", requestID, limit, cursor)
}

func (client *Client) listDialogs(ctx context.Context, target endpoint, path, requestID string, limit int, cursor string) (DialogList, error) {
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	var result DialogList
	err := client.get(ctx, target, path+"?"+query.Encode(), requestID, &result)
	return result, err
}

func (client *Client) GetDialog(ctx context.Context, requestID, dialogID string) (DialogSnapshot, error) {
	if !safePathSegment(dialogID) {
		return DialogSnapshot{}, errors.New("Controller Dialog identity is invalid")
	}
	var result DialogSnapshot
	err := client.get(ctx, client.business, internalPrefix+"/dialogs/"+url.PathEscape(dialogID), requestID, &result)
	return result, err
}

// CreateDialog forwards the caller's stable identity exactly once. Transport
// failures never trigger a new command ID or an implicit retry.
func (client *Client) CreateDialog(ctx context.Context, requestID string, input CreateDialogRequest) (CreateDialogResult, error) {
	var result CreateDialogResult
	if client == nil || input.ExpectedCollectionVersion < 0 || !safePathSegment(input.CommandID) || !safePathSegment(input.ClientInstanceID) {
		return result, errors.New("Controller command is invalid")
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return result, errors.New("Controller command is invalid")
	}
	err = client.do(ctx, client.business, http.MethodPost, internalPrefix+"/dialogs", requestID, payload, &result)
	if err != nil {
		return CreateDialogResult{}, err
	}
	if result.CommandID != input.CommandID || result.SnapshotVersion != input.ExpectedCollectionVersion+1 {
		return CreateDialogResult{}, errors.New("Controller command outcome is invalid")
	}
	return result, nil
}

func (client *Client) ListEvents(ctx context.Context, requestID string, limit int, after string) (EventList, error) {
	return client.listEvents(ctx, client.business, internalPrefix+"/events", requestID, limit, after)
}

// RecoverEvents uses the isolated recovery transport and explicit private
// endpoint. Ordinary event polling must use ListEvents instead.
func (client *Client) RecoverEvents(ctx context.Context, requestID string, limit int, after string) (EventList, error) {
	return client.listEvents(ctx, client.recovery, internalPrefix+"/recovery/events", requestID, limit, after)
}

func (client *Client) listEvents(ctx context.Context, target endpoint, path, requestID string, limit int, after string) (EventList, error) {
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if after != "" {
		query.Set("after", after)
	}
	var result EventList
	err := client.get(ctx, target, path+"?"+query.Encode(), requestID, &result)
	return result, err
}

func (client *Client) ListMessages(ctx context.Context, requestID, dialogID string, limit int, afterSequence int64) (MessageList, error) {
	if !safePathSegment(dialogID) {
		return MessageList{}, errors.New("Controller Dialog identity is invalid")
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}, "after_sequence": {strconv.FormatInt(afterSequence, 10)}}
	var result MessageList
	err := client.get(ctx, client.business, internalPrefix+"/dialogs/"+url.PathEscape(dialogID)+"/messages?"+query.Encode(), requestID, &result)
	return result, err
}

func (client *Client) ListTasks(ctx context.Context, requestID string, limit int) (TaskList, error) {
	return client.listTasks(ctx, client.business, internalPrefix+"/tasks", requestID, limit)
}

// RecoverTasks uses the isolated recovery transport and explicit private
// endpoint. Ordinary Task lists must use ListTasks instead.
func (client *Client) RecoverTasks(ctx context.Context, requestID string, limit int) (TaskList, error) {
	return client.listTasks(ctx, client.recovery, internalPrefix+"/recovery/tasks", requestID, limit)
}

func (client *Client) listTasks(ctx context.Context, target endpoint, path, requestID string, limit int) (TaskList, error) {
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	var result TaskList
	err := client.get(ctx, target, path+"?"+query.Encode(), requestID, &result)
	return result, err
}

func (client *Client) GetTask(ctx context.Context, requestID, taskID string) (TaskSnapshot, error) {
	if !safePathSegment(taskID) {
		return TaskSnapshot{}, errors.New("Controller Task identity is invalid")
	}
	var result TaskSnapshot
	err := client.get(ctx, client.control, internalPrefix+"/tasks/"+url.PathEscape(taskID), requestID, &result)
	return result, err
}

func (client *Client) Control(ctx context.Context, requestID string) (ControlSummary, error) {
	var result ControlSummary
	err := client.get(ctx, client.control, internalPrefix+"/control", requestID, &result)
	return result, err
}

func (client *Client) Health(ctx context.Context, requestID string) (Status, error) {
	var result Status
	err := client.get(ctx, client.health, internalPrefix+"/healthz", requestID, &result)
	return result, err
}

func (client *Client) Ready(ctx context.Context, requestID string) (Status, error) {
	var result Status
	err := client.get(ctx, client.health, internalPrefix+"/readyz", requestID, &result)
	return result, err
}

func (client *Client) Identity(ctx context.Context, requestID string) (Identity, error) {
	var result Identity
	err := client.get(ctx, client.health, internalPrefix+"/identity", requestID, &result)
	return result, err
}

type responseEnvelope struct {
	SchemaVersion       int             `json:"schema_version"`
	RequestID           string          `json:"request_id"`
	ServerTime          string          `json:"server_time"`
	Principal           string          `json:"principal"`
	CapabilitiesVersion string          `json:"capabilities_version"`
	Data                json.RawMessage `json:"data"`
	Error               *errorBody      `json:"error"`
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	CommandID string `json:"command_id,omitempty"`
}

func (client *Client) get(ctx context.Context, target endpoint, path, requestID string, destination any) error {
	return client.do(ctx, target, http.MethodGet, path, requestID, nil, destination)
}

func (client *Client) do(ctx context.Context, target endpoint, method, path, requestID string, body []byte, destination any) error {
	if client == nil || target.http == nil || ctx == nil || destination == nil || !validRequestID(requestID) {
		return errors.New("Controller client request is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://controller"+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("Controller client request is invalid")
	}
	request.Header.Set("X-Request-ID", requestID)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("If-Fixik-Principal", client.expectedPrincipal)
		request.Header.Set("If-Fixik-Capabilities", client.expectedCapabilities)
	}
	response, err := target.http.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return errors.New("Controller is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 599 ||
		response.Header.Get("Content-Encoding") != "" ||
		!exactJSONContentType(response.Header.Values("Content-Type")) ||
		(response.ContentLength > maximumResponseBytes) {
		return errors.New("Controller response is invalid")
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil || len(payload) == 0 || len(payload) > maximumResponseBytes {
		return errors.New("Controller response is invalid")
	}
	var envelope responseEnvelope
	if err := decodeExact(payload, &envelope); err != nil || envelope.SchemaVersion != protocolSchema ||
		envelope.RequestID != requestID || !validServerTime(envelope.ServerTime) ||
		envelope.Principal != client.expectedPrincipal ||
		envelope.CapabilitiesVersion != client.expectedCapabilities {
		return errors.New("Controller response is invalid")
	}
	if response.StatusCode != http.StatusOK && !(method == http.MethodPost && (response.StatusCode == http.StatusCreated || response.StatusCode == http.StatusAccepted)) {
		if len(envelope.Data) != 0 || envelope.Error == nil || !safeCode(envelope.Error.Code) {
			return errors.New("Controller response is invalid")
		}
		return &RemoteError{Status: response.StatusCode, Code: envelope.Error.Code}
	}
	if envelope.Error != nil || len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) {
		return errors.New("Controller response is invalid")
	}
	if err := decodeExact(envelope.Data, destination); err != nil {
		return errors.New("Controller response is invalid")
	}
	return nil
}

func decodeExact(payload []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func exactJSONContentType(values []string) bool {
	return len(values) == 1 && (values[0] == "application/json" || values[0] == "application/json; charset=utf-8")
}

func validServerTime(value string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && parsed.Location() == time.UTC && !parsed.IsZero()
}

func safePathSegment(value string) bool {
	if !utf8.ValidString(value) || value == "" || len(value) > 256 || value != strings.TrimSpace(value) ||
		strings.ContainsAny(value, "/?#") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validRequestID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' && character != '.' && character != ':' {
			return false
		}
	}
	return true
}

func safeCode(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && character != '_' {
			return false
		}
	}
	return true
}

// SocketPaths exists only for readiness diagnostics and never includes secret
// material. Callers must still avoid logging host filesystem paths publicly.
func (client *Client) SocketPaths() (string, string, string, string, error) {
	if client == nil {
		return "", "", "", "", fmt.Errorf("Controller client is not configured")
	}
	return client.business.socketPath, client.health.socketPath, client.control.socketPath, client.recovery.socketPath, nil
}
