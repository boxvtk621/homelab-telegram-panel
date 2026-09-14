// Package httpapi exposes the owner-scoped, read-only private R01 API.
package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/store"
)

const OwnerHeader = "X-Agent-Service-Owner"

type inventoryStore interface {
	CheckSchema(context.Context) error
	ListInventory(context.Context, string, string, int) (store.ListResult, error)
	ListDialogBindings(context.Context, string, string, string, int) (store.BindingListResult, error)
}

type Server struct {
	store inventoryStore
}

func New(database inventoryStore) (*Server, error) {
	if database == nil {
		return nil, errors.New("inventory store is required")
	}
	return &Server{store: database}, nil
}

type safeError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int, code, message string, retryable bool) {
	reply(w, status, map[string]safeError{"error": {Code: code, Message: message, Retryable: retryable}})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "Метод не поддерживается.", false)
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.URL.RawPath != "" {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	switch r.URL.Path {
	case "/internal/v1/healthz":
		if r.URL.RawQuery != "" {
			fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
			return
		}
		if s.store.CheckSchema(r.Context()) != nil {
			fail(w, http.StatusServiceUnavailable, "database_unavailable", "Хранилище реестра недоступно.", true)
			return
		}
		reply(w, http.StatusOK, map[string]string{"service": "up"})
	case "/internal/v1/inventory":
		s.inventory(w, r)
	case "/internal/v1/dialog-bindings":
		s.dialogBindings(w, r)
	default:
		fail(w, http.StatusNotFound, "not_found", "Объект не найден.", false)
	}
}

func (s *Server) inventory(w http.ResponseWriter, r *http.Request) {
	owners := r.Header.Values(OwnerHeader)
	if len(owners) != 1 || !model.ValidActor(owners[0]) {
		fail(w, http.StatusForbidden, "owner_scope_required", "Владелец запроса не подтверждён.", false)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	for key, values := range query {
		if (key != "limit" && key != "cursor") || len(values) != 1 {
			fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
			return
		}
	}
	limit := 50
	if values, present := query["limit"]; present {
		raw := values[0]
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 || strconv.Itoa(limit) != raw {
			fail(w, http.StatusBadRequest, "invalid_request", "Некорректный размер страницы.", false)
			return
		}
	}
	after := ""
	if values, present := query["cursor"]; present {
		raw := values[0]
		if raw == "" {
			fail(w, http.StatusBadRequest, "invalid_cursor", "Курсор списка недействителен.", false)
			return
		}
		after, err = decodeInventoryCursor(raw, owners[0])
		if err != nil {
			fail(w, http.StatusBadRequest, "invalid_cursor", "Курсор списка недействителен.", false)
			return
		}
	}
	result, err := s.store.ListInventory(r.Context(), owners[0], after, limit)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "inventory_unavailable", "Реестр временно недоступен.", true)
		return
	}
	page := model.InventoryPage{SchemaID: model.InventorySchemaID, Items: result.Items}
	if result.HasMore {
		cursor := encodeInventoryCursor(owners[0], result.After)
		page.NextCursor = &cursor
	}
	reply(w, http.StatusOK, page)
}

func (s *Server) dialogBindings(w http.ResponseWriter, r *http.Request) {
	owners := r.Header.Values(OwnerHeader)
	if len(owners) != 1 || !model.ValidActor(owners[0]) {
		fail(w, http.StatusForbidden, "owner_scope_required", "Владелец запроса не подтверждён.", false)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	for key, values := range query {
		if (key != "nodeId" && key != "limit" && key != "cursor") || len(values) != 1 {
			fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
			return
		}
	}
	nodeID := query.Get("nodeId")
	if !model.ValidUUID(nodeID) {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный агент.", false)
		return
	}
	limit := 50
	if values, present := query["limit"]; present {
		raw := values[0]
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 || strconv.Itoa(limit) != raw {
			fail(w, http.StatusBadRequest, "invalid_request", "Некорректный размер страницы.", false)
			return
		}
	}
	after := ""
	if values, present := query["cursor"]; present {
		raw := values[0]
		if raw == "" {
			fail(w, http.StatusBadRequest, "invalid_cursor", "Курсор списка недействителен.", false)
			return
		}
		after, err = decodeBindingCursor(raw, owners[0], nodeID)
		if err != nil {
			fail(w, http.StatusBadRequest, "invalid_cursor", "Курсор списка недействителен.", false)
			return
		}
	}
	result, err := s.store.ListDialogBindings(r.Context(), owners[0], nodeID, after, limit)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "inventory_unavailable", "Реестр временно недоступен.", true)
		return
	}
	page := model.DialogBindingPage{SchemaID: model.BindingsSchemaID, NodeID: nodeID, Items: result.Items}
	if result.HasMore {
		cursor := encodeBindingCursor(owners[0], nodeID, result.After)
		page.NextCursor = &cursor
	}
	reply(w, http.StatusOK, page)
}

type cursor struct {
	Owner string `json:"owner"`
	After string `json:"after"`
}

func encodeInventoryCursor(owner, after string) string {
	raw, _ := json.Marshal(cursor{Owner: owner, After: after})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeInventoryCursor(value, owner string) (string, error) {
	if len(value) > 512 {
		return "", errors.New("invalid cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != value || len(raw) > 512 {
		return "", errors.New("invalid cursor")
	}
	var decoded cursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&decoded) != nil || decoder.Decode(new(any)) != io.EOF ||
		decoded.Owner != owner || !model.ValidUUID(decoded.After) {
		return "", errors.New("invalid cursor")
	}
	return decoded.After, nil
}

type bindingCursor struct {
	Owner  string `json:"owner"`
	NodeID string `json:"nodeId"`
	After  string `json:"after"`
}

func encodeBindingCursor(owner, nodeID, after string) string {
	raw, _ := json.Marshal(bindingCursor{Owner: owner, NodeID: nodeID, After: after})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeBindingCursor(value, owner, nodeID string) (string, error) {
	if len(value) > 512 {
		return "", errors.New("invalid cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != value || len(raw) > 512 {
		return "", errors.New("invalid cursor")
	}
	var decoded bindingCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&decoded) != nil || decoder.Decode(new(any)) != io.EOF ||
		decoded.Owner != owner || decoded.NodeID != nodeID || !model.ValidUUID(decoded.After) {
		return "", errors.New("invalid cursor")
	}
	return decoded.After, nil
}
