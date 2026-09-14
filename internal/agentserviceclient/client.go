// Package agentserviceclient is the stdlib-only, read-only private client used
// by Panel. It cannot reach PostgreSQL, Docker, signing keys, or secret stores.
package agentserviceclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

const (
	OwnerHeader     = "X-Agent-Service-Owner"
	InventorySchema = "agent-management-v1"
	BindingsSchema  = "agent-dialog-bindings-v1"
	maximumBody     = 2 << 20
)

var (
	actorPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)
	uuidPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
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
	http *http.Client
}

func New(socket string) (*Client, error) {
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
	return &Client{http: &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}, nil
}

func (c *Client) Close() {
	if transport, ok := c.http.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
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

func (c *Client) read(ctx context.Context, owner, path string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent-service"+path, nil)
	if err != nil {
		return nil, &Fault{Status: http.StatusServiceUnavailable, Code: "inventory_unavailable", Retryable: true}
	}
	request.Header.Set(OwnerHeader, owner)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, &Fault{Status: http.StatusServiceUnavailable, Code: "inventory_unavailable", Retryable: true}
	}
	defer response.Body.Close()
	mediaType, _, contentTypeErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumBody+1))
	if contentTypeErr != nil || mediaType != "application/json" || readErr != nil || len(body) > maximumBody || !strictjson.Valid(body) {
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

func safeCode(value string) string {
	switch value {
	case "invalid_request", "invalid_cursor", "owner_scope_required", "not_found",
		"database_unavailable", "inventory_unavailable":
		return value
	default:
		return "inventory_unavailable"
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
