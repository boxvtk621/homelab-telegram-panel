// Package mobilegateway implements the public, non-authoritative Mobile
// Workspace boundary. It must reach business state only through Controller
// application ports.
package mobilegateway

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobileauth"
)

const (
	SessionCookieName    = "__Host-fixik_session"
	BootstrapCookieName  = "__Host-fixik_bootstrap"
	CSRFHeaderName       = "X-Fixik-CSRF"
	publicSchemaVersion  = 1
	maximumAuthBody      = 16 * 1024
	clientInstanceHeader = "X-Fixik-Client-Instance"
)

// IDGenerator returns opaque request identities.
type IDGenerator interface {
	New(string) (string, error)
}

// TelegramVerifier is the narrow signature/allowlist port.
type TelegramVerifier interface {
	Verify(string) (mobileauth.TelegramIdentity, error)
	SignatureEnvironment() mobileauth.Environment
}

// AuthConfig contains one exact public origin, the process-wide capacity gate
// shared by every public API handler and the mandatory bounded auth audit sink.
type AuthConfig struct {
	PublicOrigin          string
	Capacity              *CapacityGate
	Audit                 AuthAuditor
	DialogCreationEnabled bool
	TaskSubmissionEnabled bool
}

// AuthHandler owns only Telegram bootstrap and rebuildable web sessions.
type AuthHandler struct {
	verifier              TelegramVerifier
	sessions              *mobileauth.SessionManager
	ids                   IDGenerator
	clock                 mobileauth.Clock
	publicOrigin          string
	publicHost            string
	environment           mobileauth.Environment
	audit                 AuthAuditor
	gate                  *authGate
	capacity              *CapacityGate
	dialogCreationEnabled bool
	taskSubmissionEnabled bool
}

// NewAuthHandler constructs a fail-closed handler for one exact HTTPS origin.
func NewAuthHandler(
	config AuthConfig,
	verifier TelegramVerifier,
	sessions *mobileauth.SessionManager,
	ids IDGenerator,
	clock mobileauth.Clock,
) (*AuthHandler, error) {
	if isNilDependency(verifier) || sessions == nil || isNilDependency(ids) || isNilDependency(clock) || config.Capacity == nil || isNilDependency(config.Audit) {
		return nil, errors.New("mobile auth handler dependency is not configured")
	}
	environment := verifier.SignatureEnvironment()
	if environment != mobileauth.EnvironmentTest && environment != mobileauth.EnvironmentProduction {
		return nil, errors.New("mobile auth handler Telegram environment is invalid")
	}
	origin, err := url.Parse(config.PublicOrigin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.Hostname() == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return nil, errors.New("mobile public origin must be one exact HTTPS origin")
	}
	return &AuthHandler{
		verifier: verifier, sessions: sessions, ids: ids, clock: clock,
		publicOrigin:          origin.Scheme + "://" + origin.Host,
		publicHost:            origin.Host,
		environment:           environment,
		audit:                 config.Audit,
		gate:                  newAuthGate(),
		capacity:              config.Capacity,
		dialogCreationEnabled: config.DialogCreationEnabled,
		taskSubmissionEnabled: config.TaskSubmissionEnabled,
	}, nil
}

func isNilDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// Register installs the bounded public authentication routes.
func (handler *AuthHandler) Register(mux *http.ServeMux) error {
	if handler == nil || mux == nil {
		return errors.New("mobile auth handler or mux is not configured")
	}
	handler.registerExactMethod(mux, "/api/v1/auth/bootstrap", handler.bootstrap)
	handler.registerExactMethod(mux, "/api/v1/auth/telegram", handler.exchange)
	handler.registerExactMethod(mux, "/api/v1/session", handler.revoke)
	handler.registerExactMethod(mux, "/api/v1/session/resume", handler.resume)
	return nil
}

func (handler *AuthHandler) registerExactMethod(mux *http.ServeMux, path string, endpoint http.HandlerFunc) {
	mux.HandleFunc(path, endpoint)
}

func (handler *AuthHandler) bootstrap(response http.ResponseWriter, request *http.Request) {
	requestID, release, ok := handler.begin(response, request, authRouteBootstrap)
	if !ok {
		return
	}
	defer release()
	clientInstanceID, ok := exactClientInstance(request)
	if !ok {
		handler.writeError(response, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	token, expiresAt, err := handler.sessions.IssueBootstrap(clientInstanceID)
	if err != nil {
		handler.writeError(response, http.StatusServiceUnavailable, requestID, "auth_unavailable")
		return
	}
	maxAge := cookieMaxAge(expiresAt, handler.clock.Now())
	http.SetCookie(response, secureCookie(BootstrapCookieName, token, expiresAt, maxAge))
	handler.writeData(response, http.StatusOK, requestID, map[string]any{"expires_in": maxAge})
}

func (handler *AuthHandler) exchange(response http.ResponseWriter, request *http.Request) {
	requestID, release, ok := handler.begin(response, request, authRouteExchange)
	if !ok {
		return
	}
	defer release()
	if !contentTypeIsInitData(request.Header.Values("Content-Type")) {
		handler.writeError(response, http.StatusUnsupportedMediaType, requestID, "invalid_request")
		return
	}
	clientInstanceID, ok := exactClientInstance(request)
	if !ok {
		handler.writeError(response, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	initData, err := readInitData(response, request)
	if err != nil {
		handler.writeError(response, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	bootstrap, err := exactCookie(request, BootstrapCookieName)
	clearCookie(response, BootstrapCookieName)
	if err != nil || handler.sessions.ConsumeBootstrap(bootstrap.Value, clientInstanceID) != nil {
		handler.writeAuthenticationFailure(response, request, requestID, nil)
		return
	}
	identity, err := handler.verifier.Verify(initData)
	if err != nil {
		handler.writeAuthenticationFailure(response, request, requestID, nil)
		return
	}
	token, csrfToken, session, err := handler.sessions.Exchange(identity, clientInstanceID)
	if err != nil {
		handler.writeAuthenticationFailure(response, request, requestID, &identity)
		return
	}
	if err := handler.recordAuthAudit(request, AuthAuditAccepted, &identity); err != nil {
		_ = handler.sessions.Revoke(token)
		handler.writeError(response, http.StatusServiceUnavailable, requestID, "auth_unavailable")
		return
	}
	maxAge := cookieMaxAge(session.ExpiresAt, handler.clock.Now())
	http.SetCookie(response, secureCookie(SessionCookieName, token, session.ExpiresAt, maxAge))
	handler.writeSession(response, requestID, session, csrfToken)
}

func (handler *AuthHandler) writeSession(response http.ResponseWriter, requestID string, session mobileauth.Session, csrfToken string) {
	data := map[string]any{
		"role":               session.Role,
		"capability_version": session.CapabilityVersion,
		"expires_at":         session.ExpiresAt.UTC().Format(time.RFC3339),
		"csrf_token":         csrfToken,
	}
	if handler.dialogCreationEnabled {
		data["dialog_creation_enabled"] = true
	}
	if handler.taskSubmissionEnabled {
		data["task_submission_enabled"] = true
	}
	handler.writeData(response, http.StatusOK, requestID, data)
}

// resume recovers a CSRF nonce after WebView reload without replaying initData.
// Exact Origin plus the session cookie and bound client namespace are required;
// no replacement authentication cookie is issued, and absolute expiry is fixed.
func (handler *AuthHandler) resume(w http.ResponseWriter, r *http.Request) {
	if !exactAuthRequest(r, false) {
		setSecurityHeaders(w)
		handler.writeError(w, http.StatusBadRequest, "request-rejected", "invalid_request")
		return
	}
	// Share the existing bounded POST authentication capacity/rate budget.
	requestID, release, ok := handler.begin(w, r, authRouteExchange)
	if !ok {
		return
	}
	defer release()
	clientID, ok := exactClientInstance(r)
	cookie, err := exactCookie(r, SessionCookieName)
	if !ok || err != nil {
		handler.writeError(w, http.StatusUnauthorized, requestID, "authentication_failed")
		return
	}
	csrf, session, err := handler.sessions.Resume(cookie.Value, clientID)
	if err != nil {
		handler.writeError(w, http.StatusUnauthorized, requestID, "authentication_failed")
		return
	}
	handler.writeSession(w, requestID, session, csrf)
}

func (handler *AuthHandler) writeAuthenticationFailure(response http.ResponseWriter, request *http.Request, requestID string, identity *mobileauth.TelegramIdentity) {
	if err := handler.recordAuthAudit(request, AuthAuditRejected, identity); err != nil {
		handler.writeError(response, http.StatusServiceUnavailable, requestID, "auth_unavailable")
		return
	}
	handler.writeError(response, http.StatusUnauthorized, requestID, "authentication_failed")
}

func (handler *AuthHandler) recordAuthAudit(request *http.Request, outcome AuthAuditOutcome, identity *mobileauth.TelegramIdentity) error {
	record := AuthAuditRecord{Outcome: outcome, Environment: handler.environment}
	if identity != nil {
		record.UserID = identity.UserID
		record.AuthDate = identity.AuthDate
	}
	return handler.audit.Record(request.Context(), record)
}

func (handler *AuthHandler) revoke(response http.ResponseWriter, request *http.Request) {
	requestID, release, ok := handler.begin(response, request, authRouteRevoke)
	if !ok {
		return
	}
	defer release()
	sessionCookie, csrfToken, err := mutationCredentials(request)
	if err != nil {
		clearCookie(response, SessionCookieName)
		handler.writeError(response, http.StatusUnauthorized, requestID, "authentication_failed")
		return
	}
	if _, err := handler.sessions.AuthenticateMutation(sessionCookie.Value, csrfToken); err != nil {
		clearCookie(response, SessionCookieName)
		handler.writeError(response, http.StatusUnauthorized, requestID, "authentication_failed")
		return
	}
	_ = handler.sessions.Revoke(sessionCookie.Value)
	clearCookie(response, SessionCookieName)
	handler.writeData(response, http.StatusOK, requestID, map[string]any{"revoked": true})
}

// Authenticate returns server-derived session context for later public routes.
func (handler *AuthHandler) Authenticate(request *http.Request) (mobileauth.Session, error) {
	if handler == nil || request == nil {
		return mobileauth.Session{}, errors.New("mobile auth handler is not initialized")
	}
	if request.Host != handler.publicHost {
		return mobileauth.Session{}, errors.New("mobile request host is invalid")
	}
	cookie, err := exactCookie(request, SessionCookieName)
	if err != nil {
		return mobileauth.Session{}, errors.New("mobile session is missing")
	}
	return handler.sessions.Authenticate(cookie.Value)
}

// AuthorizeMutation validates the HttpOnly session cookie and the exact
// session-bound CSRF header. Public mutation routes still need begin's exact
// Host/Origin check before calling this method.
func (handler *AuthHandler) AuthorizeMutation(request *http.Request) (mobileauth.Session, error) {
	if handler == nil || request == nil {
		return mobileauth.Session{}, errors.New("mobile auth handler is not initialized")
	}
	if request.Host != handler.publicHost || !exactHeader(request, "Origin", handler.publicOrigin) {
		return mobileauth.Session{}, errors.New("mobile mutation origin is invalid")
	}
	cookie, csrfToken, err := mutationCredentials(request)
	if err != nil {
		return mobileauth.Session{}, err
	}
	return handler.sessions.AuthenticateMutation(cookie.Value, csrfToken)
}

func mutationCredentials(request *http.Request) (*http.Cookie, string, error) {
	if request == nil {
		return nil, "", errors.New("mobile mutation request is missing")
	}
	values := request.Header.Values(CSRFHeaderName)
	if len(values) != 1 || values[0] == "" {
		return nil, "", errors.New("mobile CSRF header is missing")
	}
	cookie, err := exactCookie(request, SessionCookieName)
	if err != nil {
		return nil, "", errors.New("mobile session is missing")
	}
	return cookie, values[0], nil
}

func exactClientInstance(request *http.Request) (string, bool) {
	values := request.Header.Values(clientInstanceHeader)
	returnValue := ""
	if len(values) == 1 {
		returnValue = values[0]
	}
	if !mobileauth.ValidClientInstanceID(returnValue) {
		return "", false
	}
	return returnValue, true
}

// exactAuthRequest rejects query/body ambiguity before any bootstrap token,
// session, replay fingerprint, verifier call, or revocation can be mutated.
func exactAuthRequest(request *http.Request, bodyRequired bool) bool {
	if request == nil || request.URL == nil || request.URL.RawPath != "" ||
		request.URL.RawQuery != "" || request.URL.ForceQuery {
		return false
	}
	if !bodyRequired && (request.ContentLength != 0 || len(request.TransferEncoding) != 0) {
		return false
	}
	return true
}

func (handler *AuthHandler) begin(response http.ResponseWriter, request *http.Request, route authRoute) (string, func(), bool) {
	setSecurityHeaders(response)
	expectedMethod, requireOrigin, bodyRequired, routeOK := authRouteContract(route)
	if !routeOK || request == nil || request.URL == nil {
		handler.writeError(response, http.StatusBadRequest, "request-rejected", "invalid_request")
		return "", nil, false
	}
	if request.Method != expectedMethod {
		response.Header().Set("Allow", expectedMethod)
		handler.writeError(response, http.StatusMethodNotAllowed, "request-rejected", "method_not_allowed")
		return "", nil, false
	}
	// Reject cheap, non-canonical public input before it can burn the fixed
	// process-wide auth rate budget. Correctly shaped requests still consume a
	// general slot and the canonical route budget; source-aware Internet abuse
	// protection remains an explicit edge gate.
	if request.Host != handler.publicHost {
		handler.writeError(response, http.StatusBadRequest, "request-rejected", "invalid_request")
		return "", nil, false
	}
	if requireOrigin && !exactHeader(request, "Origin", handler.publicOrigin) {
		handler.writeError(response, http.StatusForbidden, "request-rejected", "request_denied")
		return "", nil, false
	}
	if !exactAuthRequest(request, bodyRequired) {
		handler.writeError(response, http.StatusBadRequest, "request-rejected", "invalid_request")
		return "", nil, false
	}
	release, admitted := handler.capacity.Acquire(CapacityGeneral)
	if !admitted {
		response.Header().Set("Retry-After", "1")
		handler.writeError(response, http.StatusTooManyRequests, "request-rejected", "rate_limited")
		return "", nil, false
	}
	if !handler.gate.admit(route, handler.clock.Now()) {
		response.Header().Set("Retry-After", "60")
		handler.writeError(response, http.StatusTooManyRequests, "request-rejected", "rate_limited")
		release()
		return "", nil, false
	}
	requestID, err := handler.ids.New("request")
	if err != nil || !validRequestID(requestID) {
		handler.writeError(response, http.StatusServiceUnavailable, "request-unavailable", "request_id_unavailable")
		release()
		return "", nil, false
	}
	return requestID, release, true
}

func authRouteContract(route authRoute) (method string, requireOrigin, bodyRequired, ok bool) {
	switch route {
	case authRouteBootstrap:
		return http.MethodGet, false, false, true
	case authRouteExchange:
		return http.MethodPost, true, true, true
	case authRouteRevoke:
		return http.MethodDelete, true, false, true
	default:
		return "", false, false, false
	}
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

func readInitData(response http.ResponseWriter, request *http.Request) (string, error) {
	body := http.MaxBytesReader(response, request.Body, maximumAuthBody)
	defer body.Close()
	payload, err := io.ReadAll(body)
	if err != nil || len(payload) == 0 {
		return "", errors.New("invalid init data body")
	}
	return string(payload), nil
}

func contentTypeIsInitData(values []string) bool {
	if len(values) != 1 {
		return false
	}
	mediaType, parameters, err := mime.ParseMediaType(values[0])
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		return false
	}
	for key, value := range parameters {
		if key != "charset" || !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	return true
}

func exactHeader(request *http.Request, name, expected string) bool {
	values := request.Header.Values(name)
	return len(values) == 1 && values[0] == expected
}

func exactCookie(request *http.Request, name string) (*http.Cookie, error) {
	var matched *http.Cookie
	for _, cookie := range request.Cookies() {
		if cookie.Name != name {
			continue
		}
		if matched != nil || cookie.Value == "" {
			return nil, errors.New("duplicate or empty mobile cookie")
		}
		matched = cookie
	}
	if matched == nil {
		return nil, http.ErrNoCookie
	}
	return matched, nil
}

func secureCookie(name, value string, expires time.Time, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/", Expires: expires,
		MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	}
}

func cookieMaxAge(expiresAt, now time.Time) int {
	remaining := expiresAt.Sub(now)
	if remaining <= 0 {
		return 0
	}
	return int((remaining + time.Second - 1) / time.Second)
}

func clearCookie(response http.ResponseWriter, name string) {
	http.SetCookie(response, secureCookie(name, "", time.Unix(1, 0).UTC(), -1))
}

func setSecurityHeaders(response http.ResponseWriter) {
	header := response.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	header.Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
}

type envelope struct {
	SchemaVersion int            `json:"schema_version"`
	RequestID     string         `json:"request_id"`
	ServerTime    string         `json:"server_time"`
	Data          map[string]any `json:"data,omitempty"`
	Error         *errorEnvelope `json:"error,omitempty"`
}

type errorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (handler *AuthHandler) writeData(response http.ResponseWriter, status int, requestID string, data map[string]any) {
	handler.writeEnvelope(response, status, envelope{
		SchemaVersion: publicSchemaVersion,
		RequestID:     requestID,
		ServerTime:    handler.clock.Now().UTC().Format("2006-01-02T15:04:05.000000Z"),
		Data:          data,
	})
}

func (handler *AuthHandler) writeError(response http.ResponseWriter, status int, requestID, code string) {
	handler.writeEnvelope(response, status, envelope{
		SchemaVersion: publicSchemaVersion,
		RequestID:     requestID,
		ServerTime:    handler.clock.Now().UTC().Format("2006-01-02T15:04:05.000000Z"),
		Error:         &errorEnvelope{Code: code, Message: "request could not be completed"},
	})
}

func (handler *AuthHandler) writeEnvelope(response http.ResponseWriter, status int, value envelope) {
	payload, err := json.Marshal(value)
	if err != nil {
		http.Error(response, "response unavailable", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_, _ = response.Write(append(payload, '\n'))
}
