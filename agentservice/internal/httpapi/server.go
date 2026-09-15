// Package httpapi exposes the owner-scoped private agent-service API.
package httpapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/exactjson"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/registry"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/store"
)

const (
	OwnerHeader       = "X-Agent-Service-Owner"
	WorkerTokenHeader = "X-Agent-Service-Worker-Token"
)

type serviceStore interface {
	CheckSchema(context.Context) error
	ListInventory(context.Context, string, string, int) (store.ListResult, error)
	ListDialogBindings(context.Context, string, string, string, int) (store.BindingListResult, error)
	AcceptOperation(context.Context, string, model.OperationIntent) (store.AcceptResult, error)
	GetOperation(context.Context, string, string) (model.OperationStatus, error)
	GetOperationTarget(context.Context, string, string) (model.OperationTargetStatus, error)
	ClaimNextOperation(context.Context, string, string, string, time.Duration) (store.ClaimedOperation, error)
	CheckOperationAuthority(context.Context, string, string, store.OperationClaim) error
	MarkOperationSent(context.Context, string, store.OperationClaim) (store.OperationClaim, error)
	AdvanceOperation(context.Context, string, store.OperationClaim, store.OperationUpdate) (store.OperationClaim, error)
	GetRegistryEnvelope(context.Context, string) ([]byte, int64, string, error)
	ReserveRegistryOperation(context.Context, string, model.RegistryOperationIntent, registry.Verified, []string, string) (store.RegistryReserveResult, error)
	GetRegistryOperation(context.Context, string, string) (model.RegistryOperationStatus, error)
	GetRegistryOperationIntent(context.Context, string, string) (model.RegistryOperationIntent, error)
	MarkRegistryOperationSent(context.Context, string, model.RegistryOperationCommand) (model.RegistryOperationStatus, error)
	MarkRegistryOperationUnknown(context.Context, string, model.RegistryOperationCommand) (model.RegistryOperationStatus, error)
	FailRegistryOperation(context.Context, string, model.RegistryOperationFailure) (model.RegistryOperationStatus, error)
	FinishRegistryOperation(context.Context, string, model.RegistryOperationFinish, registry.Verified) (model.RegistryOperationStatus, error)
}

type Server struct {
	store          serviceStore
	workerToken    string
	registrySigner []byte
}

type registryOperationIntentShape struct {
	SchemaID      string                 `json:"schemaId"`
	OperationID   string                 `json:"operationId"`
	Expected      model.RegistryExpected `json:"expected"`
	Registry      any                    `json:"registry"`
	NewNodeHostID *string                `json:"newNodeHostId"`
}

func New(database serviceStore) (*Server, error) {
	if database == nil {
		return nil, errors.New("inventory store is required")
	}
	return &Server{store: database}, nil
}

func NewWithWorkerToken(database serviceStore, workerToken string) (*Server, error) {
	return NewWithCapabilities(database, workerToken, nil)
}

func NewWithCapabilities(database serviceStore, workerToken string, registrySigner []byte) (*Server, error) {
	server, err := New(database)
	if err != nil {
		return nil, err
	}
	if len(workerToken) < 32 || len(workerToken) > 128 || strings.TrimSpace(workerToken) != workerToken || strings.ContainsAny(workerToken, "\x00\r\n") {
		return nil, errors.New("invalid operation worker token")
	}
	if len(registrySigner) > 0 {
		if err := registry.ValidateSigner(registrySigner); err != nil {
			return nil, errors.New("invalid registry signer")
		}
		server.registrySigner = append([]byte(nil), registrySigner...)
	}
	server.workerToken = workerToken
	return server, nil
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
	if r.URL.RawPath != "" {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	switch r.URL.Path {
	case "/internal/v1/healthz":
		if !readOnlyRequest(r) {
			fail(w, methodStatus(r, http.MethodGet), "method_not_allowed", "Метод не поддерживается.", false)
			return
		}
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
		if !readOnlyRequest(r) {
			fail(w, methodStatus(r, http.MethodGet), "method_not_allowed", "Метод не поддерживается.", false)
			return
		}
		s.inventory(w, r)
	case "/internal/v1/dialog-bindings":
		if !readOnlyRequest(r) {
			fail(w, methodStatus(r, http.MethodGet), "method_not_allowed", "Метод не поддерживается.", false)
			return
		}
		s.dialogBindings(w, r)
	case "/internal/v1/operations":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "Метод не поддерживается.", false)
			return
		}
		s.acceptOperation(w, r)
	case "/internal/v1/registry-operations":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "Метод не поддерживается.", false)
			return
		}
		s.reserveRegistryOperation(w, r)
	case "/internal/v1/registry-operations/sent":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "Метод не поддерживается.", false)
			return
		}
		s.transitionRegistryOperation(w, r, "sent")
	case "/internal/v1/registry-operations/unknown":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "Метод не поддерживается.", false)
			return
		}
		s.transitionRegistryOperation(w, r, "unknown")
	case "/internal/v1/registry-operations/finish":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "Метод не поддерживается.", false)
			return
		}
		s.finishRegistryOperation(w, r)
	case "/internal/v1/registry-operations/fail":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "Метод не поддерживается.", false)
			return
		}
		s.failRegistryOperation(w, r)
	case "/internal/v1/operation-workers/claim":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "Метод не поддерживается.", false)
			return
		}
		s.claimOperation(w, r)
	case "/internal/v1/operation-workers/authority":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "Метод не поддерживается.", false)
			return
		}
		s.operationAuthority(w, r)
	case "/internal/v1/operation-workers/sent":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "Метод не поддерживается.", false)
			return
		}
		s.markOperationSent(w, r)
	case "/internal/v1/operation-workers/advance":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "Метод не поддерживается.", false)
			return
		}
		s.advanceOperation(w, r)
	default:
		if strings.HasPrefix(r.URL.Path, "/internal/v1/registry-operations/") {
			if !readOnlyRequest(r) {
				fail(w, methodStatus(r, http.MethodGet), "method_not_allowed", "Метод не поддерживается.", false)
				return
			}
			s.registryOperationStatus(w, r, strings.TrimPrefix(r.URL.Path, "/internal/v1/registry-operations/"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/internal/v1/operation-targets/") {
			if !readOnlyRequest(r) {
				fail(w, methodStatus(r, http.MethodGet), "method_not_allowed", "Метод не поддерживается.", false)
				return
			}
			s.operationTarget(w, r, strings.TrimPrefix(r.URL.Path, "/internal/v1/operation-targets/"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/internal/v1/operations/") {
			if !readOnlyRequest(r) {
				fail(w, methodStatus(r, http.MethodGet), "method_not_allowed", "Метод не поддерживается.", false)
				return
			}
			s.operationStatus(w, r, strings.TrimPrefix(r.URL.Path, "/internal/v1/operations/"))
			return
		}
		fail(w, http.StatusNotFound, "not_found", "Объект не найден.", false)
	}
}

func readOnlyRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && r.ContentLength == 0 && len(r.TransferEncoding) == 0
}

func methodStatus(r *http.Request, expected string) int {
	if r.Method != expected {
		return http.StatusMethodNotAllowed
	}
	return http.StatusBadRequest
}

func owner(r *http.Request) (string, bool) {
	owners := r.Header.Values(OwnerHeader)
	returnValue := ""
	if len(owners) == 1 {
		returnValue = owners[0]
	}
	return returnValue, len(owners) == 1 && model.ValidActor(returnValue)
}

func (s *Server) inventory(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := owner(r)
	if !ok {
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
		after, err = decodeInventoryCursor(raw, ownerID)
		if err != nil {
			fail(w, http.StatusBadRequest, "invalid_cursor", "Курсор списка недействителен.", false)
			return
		}
	}
	result, err := s.store.ListInventory(r.Context(), ownerID, after, limit)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "inventory_unavailable", "Реестр временно недоступен.", true)
		return
	}
	page := model.InventoryPage{SchemaID: model.InventorySchemaID, Items: result.Items}
	if result.HasMore {
		cursor := encodeInventoryCursor(ownerID, result.After)
		page.NextCursor = &cursor
	}
	reply(w, http.StatusOK, page)
}

func (s *Server) dialogBindings(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := owner(r)
	if !ok {
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
		after, err = decodeBindingCursor(raw, ownerID, nodeID)
		if err != nil {
			fail(w, http.StatusBadRequest, "invalid_cursor", "Курсор списка недействителен.", false)
			return
		}
	}
	result, err := s.store.ListDialogBindings(r.Context(), ownerID, nodeID, after, limit)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "inventory_unavailable", "Реестр временно недоступен.", true)
		return
	}
	page := model.DialogBindingPage{SchemaID: model.BindingsSchemaID, NodeID: nodeID, Items: result.Items}
	if result.HasMore {
		cursor := encodeBindingCursor(ownerID, nodeID, result.After)
		page.NextCursor = &cursor
	}
	reply(w, http.StatusOK, page)
}

func (s *Server) acceptOperation(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := owner(r)
	if !ok {
		fail(w, http.StatusForbidden, "owner_scope_required", "Владелец запроса не подтверждён.", false)
		return
	}
	if r.URL.RawQuery != "" || len(r.TransferEncoding) != 0 || r.ContentLength <= 0 || r.ContentLength > 64<<10 {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 || !registry.UniqueJSON(raw) || !exactjson.Shape(raw, model.OperationIntent{}) {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	var intent model.OperationIntent
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&intent) != nil || decoder.Decode(new(any)) != io.EOF || model.ValidateOperationIntent(intent) != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	result, err := s.store.AcceptOperation(r.Context(), ownerID, intent)
	if errors.Is(err, store.ErrOperationConflict) {
		fail(w, http.StatusConflict, "operation_conflict", "Версия или идентификатор операции устарели.", false)
		return
	}
	if errors.Is(err, store.ErrOperationNotFound) {
		fail(w, http.StatusNotFound, "not_found", "Объект не найден.", false)
		return
	}
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "operation_unavailable", "Операция временно недоступна.", true)
		return
	}
	reply(w, http.StatusAccepted, result.Receipt)
}

func (s *Server) operationStatus(w http.ResponseWriter, r *http.Request, operationID string) {
	ownerID, ok := owner(r)
	if !ok {
		fail(w, http.StatusForbidden, "owner_scope_required", "Владелец запроса не подтверждён.", false)
		return
	}
	if r.URL.RawQuery != "" || !model.ValidActor(operationID) || strings.Contains(operationID, "/") {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	status, err := s.store.GetOperation(r.Context(), ownerID, operationID)
	if errors.Is(err, store.ErrOperationNotFound) {
		fail(w, http.StatusNotFound, "not_found", "Объект не найден.", false)
		return
	}
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "operation_unavailable", "Операция временно недоступна.", true)
		return
	}
	reply(w, http.StatusOK, status)
}

func (s *Server) operationTarget(w http.ResponseWriter, r *http.Request, nodeID string) {
	ownerID, ok := owner(r)
	if !ok {
		fail(w, http.StatusForbidden, "owner_scope_required", "Владелец запроса не подтверждён.", false)
		return
	}
	if r.URL.RawQuery != "" || !model.ValidUUID(nodeID) || strings.Contains(nodeID, "/") {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	target, err := s.store.GetOperationTarget(r.Context(), ownerID, nodeID)
	if errors.Is(err, store.ErrOperationNotFound) {
		fail(w, http.StatusNotFound, "not_found", "Объект не найден.", false)
		return
	}
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "operation_unavailable", "Операция временно недоступна.", true)
		return
	}
	reply(w, http.StatusOK, target)
}

func (s *Server) reserveRegistryOperation(w http.ResponseWriter, r *http.Request) {
	if !s.workerAuthorized(r) {
		fail(w, http.StatusForbidden, "worker_scope_required", "Исполнитель операции не подтверждён.", false)
		return
	}
	if len(s.registrySigner) == 0 {
		fail(w, http.StatusServiceUnavailable, "registry_operations_unavailable", "Операции реестра не настроены.", false)
		return
	}
	ownerID, ok := owner(r)
	if !ok {
		fail(w, http.StatusForbidden, "owner_scope_required", "Владелец запроса не подтверждён.", false)
		return
	}
	var intent model.RegistryOperationIntent
	if !decodeExactBody(r, &intent, registryOperationIntentShape{}, 320<<10) || model.ValidateRegistryOperationIntent(intent) != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	requestHash, err := model.RegistryOperationRequestHash(intent)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	if existing, statusErr := s.store.GetRegistryOperation(r.Context(), ownerID, intent.OperationID); statusErr == nil {
		if existing.Receipt.RequestHash != requestHash {
			fail(w, http.StatusConflict, "registry_operation_conflict", "Версия или идентификатор операции устарели.", false)
			return
		}
		reply(w, http.StatusAccepted, existing)
		return
	} else if !errors.Is(statusErr, store.ErrOperationNotFound) {
		fail(w, http.StatusServiceUnavailable, "registry_operation_unavailable", "Операция реестра временно недоступна.", true)
		return
	}
	candidate, err := registry.Verify(intent.Registry, s.registrySigner)
	if err != nil || candidate.Manifest.OwnerID != ownerID {
		fail(w, http.StatusBadRequest, "registry_rejected", "Подписанный реестр отклонён.", false)
		return
	}
	currentRaw, currentVersion, currentHash, err := s.store.GetRegistryEnvelope(r.Context(), ownerID)
	if errors.Is(err, store.ErrOperationNotFound) {
		fail(w, http.StatusNotFound, "not_found", "Объект не найден.", false)
		return
	}
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "registry_operation_unavailable", "Операция реестра временно недоступна.", true)
		return
	}
	current, err := registry.Verify(currentRaw, s.registrySigner)
	if err != nil || current.Manifest.OwnerID != ownerID || current.Manifest.RegistryVersion != currentVersion ||
		current.ManifestSHA256 != currentHash || intent.Expected.RegistryVersion != currentVersion ||
		intent.Expected.RegistrySHA256 != currentHash {
		fail(w, http.StatusConflict, "registry_operation_conflict", "Версия или идентификатор операции устарели.", false)
		return
	}
	affected, newNodeID, err := registry.ProjectionDelta(current.Manifest, candidate.Manifest)
	if err != nil {
		fail(w, http.StatusConflict, "registry_operation_conflict", "Проекция реестра не является допустимым изменением.", false)
		return
	}
	slices.Sort(affected)
	result, err := s.store.ReserveRegistryOperation(r.Context(), ownerID, intent, candidate, affected, newNodeID)
	if !registryOperationError(w, err) {
		return
	}
	reply(w, http.StatusAccepted, result.Status)
}

func (s *Server) registryOperationStatus(w http.ResponseWriter, r *http.Request, operationID string) {
	if !s.workerAuthorized(r) {
		fail(w, http.StatusForbidden, "worker_scope_required", "Исполнитель операции не подтверждён.", false)
		return
	}
	ownerID, ok := owner(r)
	if !ok {
		fail(w, http.StatusForbidden, "owner_scope_required", "Владелец запроса не подтверждён.", false)
		return
	}
	if r.URL.RawQuery != "" || !model.ValidActor(operationID) || strings.Contains(operationID, "/") {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	status, err := s.store.GetRegistryOperation(r.Context(), ownerID, operationID)
	if !registryOperationError(w, err) {
		return
	}
	reply(w, http.StatusOK, status)
}

func (s *Server) transitionRegistryOperation(w http.ResponseWriter, r *http.Request, transition string) {
	if !s.workerAuthorized(r) {
		fail(w, http.StatusForbidden, "worker_scope_required", "Исполнитель операции не подтверждён.", false)
		return
	}
	ownerID, ok := owner(r)
	if !ok {
		fail(w, http.StatusForbidden, "owner_scope_required", "Владелец запроса не подтверждён.", false)
		return
	}
	var command model.RegistryOperationCommand
	if !decodeExactBody(r, &command, model.RegistryOperationCommand{}, 64<<10) || model.ValidateRegistryOperationCommand(command) != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	var status model.RegistryOperationStatus
	var err error
	if transition == "sent" {
		status, err = s.store.MarkRegistryOperationSent(r.Context(), ownerID, command)
	} else {
		status, err = s.store.MarkRegistryOperationUnknown(r.Context(), ownerID, command)
	}
	if !registryOperationError(w, err) {
		return
	}
	reply(w, http.StatusOK, status)
}

func (s *Server) finishRegistryOperation(w http.ResponseWriter, r *http.Request) {
	if !s.workerAuthorized(r) {
		fail(w, http.StatusForbidden, "worker_scope_required", "Исполнитель операции не подтверждён.", false)
		return
	}
	if len(s.registrySigner) == 0 {
		fail(w, http.StatusServiceUnavailable, "registry_operations_unavailable", "Операции реестра не настроены.", false)
		return
	}
	ownerID, ok := owner(r)
	if !ok {
		fail(w, http.StatusForbidden, "owner_scope_required", "Владелец запроса не подтверждён.", false)
		return
	}
	var finish model.RegistryOperationFinish
	if !decodeExactBody(r, &finish, model.RegistryOperationFinish{}, 64<<10) || model.ValidateRegistryOperationFinish(finish) != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	intent, err := s.store.GetRegistryOperationIntent(r.Context(), ownerID, finish.OperationID)
	if !registryOperationError(w, err) {
		return
	}
	candidate, err := registry.Verify(intent.Registry, s.registrySigner)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "registry_operation_unavailable", "Операция реестра временно недоступна.", false)
		return
	}
	status, err := s.store.FinishRegistryOperation(r.Context(), ownerID, finish, candidate)
	if !registryOperationError(w, err) {
		return
	}
	reply(w, http.StatusOK, status)
}

func (s *Server) failRegistryOperation(w http.ResponseWriter, r *http.Request) {
	if !s.workerAuthorized(r) {
		fail(w, http.StatusForbidden, "worker_scope_required", "Исполнитель операции не подтверждён.", false)
		return
	}
	ownerID, ok := owner(r)
	if !ok {
		fail(w, http.StatusForbidden, "owner_scope_required", "Владелец запроса не подтверждён.", false)
		return
	}
	var failure model.RegistryOperationFailure
	if !decodeExactBody(r, &failure, model.RegistryOperationFailure{}, 64<<10) || model.ValidateRegistryOperationFailure(failure) != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	status, err := s.store.FailRegistryOperation(r.Context(), ownerID, failure)
	if !registryOperationError(w, err) {
		return
	}
	reply(w, http.StatusOK, status)
}

func (s *Server) claimOperation(w http.ResponseWriter, r *http.Request) {
	if !s.workerAuthorized(r) {
		fail(w, http.StatusForbidden, "worker_scope_required", "Исполнитель операции не подтверждён.", false)
		return
	}
	ownerID, ok := owner(r)
	if !ok {
		fail(w, http.StatusForbidden, "owner_scope_required", "Владелец запроса не подтверждён.", false)
		return
	}
	var request model.OperationClaimRequest
	if !decodeOperationBody(r, &request, model.OperationClaimRequest{}) || model.ValidateOperationClaimRequest(request) != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	claimed, err := s.store.ClaimNextOperation(r.Context(), ownerID, request.NodeID, request.WorkerID, time.Duration(request.LeaseMilliseconds)*time.Millisecond)
	if !operationWorkerError(w, err) {
		return
	}
	work := model.OperationWork{
		SchemaID: model.OperationWorkSchemaID, Intent: claimed.Intent,
		Proof: claimProof(claimed.Claim), EffectState: claimed.EffectState,
	}
	if model.ValidateOperationWork(work) != nil {
		fail(w, http.StatusServiceUnavailable, "operation_unavailable", "Операция временно недоступна.", true)
		return
	}
	reply(w, http.StatusOK, work)
}

func (s *Server) operationAuthority(w http.ResponseWriter, r *http.Request) {
	if !s.workerAuthorized(r) {
		fail(w, http.StatusForbidden, "worker_scope_required", "Исполнитель операции не подтверждён.", false)
		return
	}
	ownerID, ok := owner(r)
	if !ok {
		fail(w, http.StatusForbidden, "owner_scope_required", "Владелец запроса не подтверждён.", false)
		return
	}
	var request model.OperationAuthorityRequest
	if !decodeOperationBody(r, &request, model.OperationAuthorityRequest{}) || model.ValidateOperationAuthorityRequest(request) != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	claim, err := storeClaim(request.Proof)
	if err != nil || s.store.CheckOperationAuthority(r.Context(), ownerID, request.Proof.NodeID, claim) != nil {
		fail(w, http.StatusConflict, "stale_worker", "Полномочия исполнителя устарели.", false)
		return
	}
	reply(w, http.StatusOK, model.OperationAuthority{SchemaID: model.OperationAuthoritySchema, Active: true})
}

func (s *Server) markOperationSent(w http.ResponseWriter, r *http.Request) {
	if !s.workerAuthorized(r) {
		fail(w, http.StatusForbidden, "worker_scope_required", "Исполнитель операции не подтверждён.", false)
		return
	}
	ownerID, ok := owner(r)
	if !ok {
		fail(w, http.StatusForbidden, "owner_scope_required", "Владелец запроса не подтверждён.", false)
		return
	}
	var request model.OperationSentRequest
	if !decodeOperationBody(r, &request, model.OperationSentRequest{}) || model.ValidateOperationSentRequest(request) != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	claim, err := storeClaim(request.Proof)
	if err == nil {
		claim, err = s.store.MarkOperationSent(r.Context(), ownerID, claim)
	}
	if !operationWorkerError(w, err) {
		return
	}
	reply(w, http.StatusOK, claimProof(claim))
}

func (s *Server) advanceOperation(w http.ResponseWriter, r *http.Request) {
	if !s.workerAuthorized(r) {
		fail(w, http.StatusForbidden, "worker_scope_required", "Исполнитель операции не подтверждён.", false)
		return
	}
	ownerID, ok := owner(r)
	if !ok {
		fail(w, http.StatusForbidden, "owner_scope_required", "Владелец запроса не подтверждён.", false)
		return
	}
	var request model.OperationAdvanceRequest
	if !decodeOperationBody(r, &request, model.OperationAdvanceRequest{}) || model.ValidateOperationAdvanceRequest(request) != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "Некорректный запрос.", false)
		return
	}
	claim, err := storeClaim(request.Proof)
	if err == nil {
		_, err = s.store.AdvanceOperation(r.Context(), ownerID, claim, store.OperationUpdate{
			Phase: request.Phase, EffectState: request.EffectState, ResultCode: request.ResultCode,
		})
	}
	if !operationWorkerError(w, err) {
		return
	}
	status, err := s.store.GetOperation(r.Context(), ownerID, request.Proof.OperationID)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "operation_unavailable", "Операция временно недоступна.", true)
		return
	}
	reply(w, http.StatusOK, status)
}

func decodeOperationBody(r *http.Request, target, shape any) bool {
	return decodeExactBody(r, target, shape, 64<<10)
}

func decodeExactBody(r *http.Request, target, shape any, maximum int64) bool {
	if r.URL.RawQuery != "" || len(r.TransferEncoding) != 0 || r.ContentLength <= 0 || r.ContentLength > maximum {
		return false
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maximum+1))
	if err != nil || int64(len(raw)) > maximum || !registry.UniqueJSON(raw) || !exactjson.Shape(raw, shape) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target) == nil && decoder.Decode(new(any)) == io.EOF
}

func registryOperationError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, store.ErrOperationNotFound) {
		fail(w, http.StatusNotFound, "not_found", "Объект не найден.", false)
		return false
	}
	if errors.Is(err, store.ErrOperationConflict) {
		fail(w, http.StatusConflict, "registry_operation_conflict", "Версия или идентификатор операции устарели.", false)
		return false
	}
	fail(w, http.StatusServiceUnavailable, "registry_operation_unavailable", "Операция реестра временно недоступна.", true)
	return false
}

func (s *Server) workerAuthorized(r *http.Request) bool {
	values := r.Header.Values(WorkerTokenHeader)
	return len(values) == 1 && s.workerToken != "" && len(values[0]) == len(s.workerToken) &&
		subtle.ConstantTimeCompare([]byte(values[0]), []byte(s.workerToken)) == 1
}

func claimProof(claim store.OperationClaim) model.OperationClaimProof {
	return model.OperationClaimProof{
		SchemaID: model.OperationProofSchemaID, OperationID: claim.OperationID, NodeID: claim.NodeID,
		RequestHash: claim.RequestHash, Generation: claim.Generation, WorkerID: claim.WorkerID, WorkerToken: claim.Token,
		OperationVersion: claim.Version, LeaseExpiresAt: model.FormatOperationTime(claim.ExpiresAt),
	}
}

func storeClaim(proof model.OperationClaimProof) (store.OperationClaim, error) {
	if model.ValidateOperationClaimProof(proof) != nil {
		return store.OperationClaim{}, errors.New("invalid operation claim")
	}
	expires, _ := time.Parse(time.RFC3339Nano, proof.LeaseExpiresAt)
	return store.OperationClaim{
		OperationID: proof.OperationID, RequestHash: proof.RequestHash, NodeID: proof.NodeID, Generation: proof.Generation,
		WorkerID: proof.WorkerID, Token: proof.WorkerToken, Version: proof.OperationVersion, ExpiresAt: expires,
	}, nil
}

func operationWorkerError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, store.ErrOperationNotFound) {
		fail(w, http.StatusNotFound, "not_found", "Объект не найден.", false)
		return false
	}
	if errors.Is(err, store.ErrOperationConflict) || errors.Is(err, store.ErrStaleWorker) {
		fail(w, http.StatusConflict, "stale_worker", "Полномочия исполнителя устарели.", false)
		return false
	}
	fail(w, http.StatusServiceUnavailable, "operation_unavailable", "Операция временно недоступна.", true)
	return false
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
