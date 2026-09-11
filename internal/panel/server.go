package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessrouter"
	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

const edgeUserHeader = "X-Panel-Authenticated-User"

var edgeUserPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,127}$`)

type Server struct {
	cfg               Config
	static            http.Handler
	sessions          *sessions
	ownerID           string
	general, auth     chan struct{}
	mu                sync.Mutex
	bootstrapWindow   time.Time
	bootstrapAttempts int
	router            *harnessrouter.Router
	streams, control  chan struct{}
	commandBodies     chan struct{}
}

func New(cfg Config, static http.Handler) (*Server, error) {
	if static == nil {
		return nil, errors.New("missing web assets")
	}
	router, err := harnessrouter.Load(cfg.Harness, cfg.HarnessRouterState, cfg.HarnessRouterSocket)
	if err != nil {
		return nil, err
	}
	ownerID := router.OwnerID()
	if ownerID == "" {
		ownerID = cfg.OwnerID
	}
	if ownerID == "" || (cfg.OwnerID != "" && cfg.OwnerID != ownerID) {
		router.Close()
		return nil, errors.New("Panel owner identity does not match Harness registry")
	}
	return &Server{
		cfg: cfg, static: static, sessions: newSessions(), ownerID: ownerID,
		general: make(chan struct{}, 8), auth: make(chan struct{}, 2), router: router,
		streams: make(chan struct{}, 4), control: make(chan struct{}, 2), commandBodies: make(chan struct{}, 4),
	}, nil
}

func BootstrapRouter(cfg Config) error {
	return harnessrouter.Bootstrap(cfg.Harness, cfg.HarnessRouterState)
}

func PreflightHarness(ctx context.Context, cfg Config) error {
	return harnessrouter.Preflight(ctx, cfg.Harness)
}

func (s *Server) Close() {
	if s.router != nil {
		s.router.Close()
	}
	s.sessions.close()
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int, code string) {
	reply(w, status, map[string]string{"error": code})
}

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		fail(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 72<<10)
	raw, err := io.ReadAll(r.Body)
	if err != nil || !strictjson.Valid(raw) {
		fail(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil || decoder.Decode(new(any)) != io.EOF {
		fail(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	origin, _ := url.Parse(s.cfg.Origin)
	if r.Host != origin.Host || r.URL.RawPath != "" || (path.Clean(r.URL.Path) != r.URL.Path && r.URL.Path != s.cfg.BasePath+"/") || r.URL.ForceQuery || (r.Header.Get("Sec-Fetch-Site") != "" && r.Header.Get("Sec-Fetch-Site") != "same-origin" && r.Header.Get("Sec-Fetch-Site") != "none") {
		fail(w, http.StatusForbidden, "access_denied")
		return
	}
	if s.cfg.BasePath != "" {
		if r.URL.Path == s.cfg.BasePath && r.Method == http.MethodGet && r.URL.RawQuery == "" {
			http.Redirect(w, r, s.cfg.BasePath+"/", http.StatusPermanentRedirect)
			return
		}
		if !strings.HasPrefix(r.URL.Path, s.cfg.BasePath+"/") {
			http.NotFound(w, r)
			return
		}
		r = r.Clone(r.Context())
		r.URL.Path = strings.TrimPrefix(r.URL.Path, s.cfg.BasePath)
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		s.static.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.Method == http.MethodGet && (r.ContentLength != 0 || len(r.TransferEncoding) != 0) {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.Method == http.MethodPost && (len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != s.cfg.Origin || r.URL.RawQuery != "") {
		fail(w, http.StatusForbidden, "access_denied")
		return
	}
	if r.URL.Path == "/api/v2/healthz" && r.Method == http.MethodGet {
		reply(w, http.StatusOK, map[string]string{"panel": "up"})
		return
	}
	if r.URL.Path == "/api/v2/bootstrap" && r.Method == http.MethodPost {
		s.bootstrap(w, r)
		return
	}

	harnessRoute := strings.HasPrefix(r.URL.Path, "/api/v2/harness/")
	cookies := r.CookiesNamed(s.sessionCookieName())
	if len(cookies) != 1 {
		fail(w, http.StatusUnauthorized, "authentication_required")
		return
	}
	sessionID := cookies[0].Value
	var current session
	var ok bool
	if harnessRoute && r.Method == http.MethodGet {
		current, ok = s.sessions.peek(sessionID)
	} else {
		current, ok = s.sessions.get(sessionID)
	}
	if !ok {
		fail(w, http.StatusUnauthorized, "authentication_required")
		return
	}
	if r.Method == http.MethodPost && (len(r.Header.Values("X-Panel-CSRF")) != 1 || !equal(r.Header.Get("X-Panel-CSRF"), current.csrf)) {
		fail(w, http.StatusForbidden, "access_denied")
		return
	}
	if harnessRoute {
		s.harnessHTTP(w, r, sessionID, current)
		return
	}
	if r.URL.Path == "/api/v2/session" && r.Method == http.MethodGet {
		s.sessionReply(w, current)
		return
	}
	if r.URL.Path == "/api/v2/logout" && r.Method == http.MethodPost {
		s.sessions.revoke(sessionID)
		http.SetCookie(w, &http.Cookie{Name: s.sessionCookieName(), Value: "", Path: s.cfg.BasePath + "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		reply(w, http.StatusOK, map[string]bool{"logged_out": true})
		return
	}
	applyNotFound(w)
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	select {
	case s.auth <- struct{}{}:
		defer func() { <-s.auth }()
	default:
		fail(w, http.StatusTooManyRequests, "capacity_exhausted")
		return
	}
	s.mu.Lock()
	if time.Since(s.bootstrapWindow) >= time.Minute {
		s.bootstrapWindow = time.Now()
		s.bootstrapAttempts = 0
	}
	allowed := s.bootstrapAttempts < 12
	s.bootstrapAttempts++
	s.mu.Unlock()
	if !allowed {
		fail(w, http.StatusTooManyRequests, "bootstrap_rate_limited")
		return
	}
	if len(r.Header.Values(edgeUserHeader)) != 1 || !edgeUserPattern.MatchString(r.Header.Get(edgeUserHeader)) {
		fail(w, http.StatusForbidden, "edge_authentication_required")
		return
	}
	var input struct{}
	if !decode(w, r, &input) {
		return
	}
	id, current, err := s.sessions.create(s.ownerID, r.Header.Get(edgeUserHeader))
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "session_unavailable")
		return
	}
	for _, cookie := range r.CookiesNamed(s.sessionCookieName()) {
		s.sessions.revoke(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: s.sessionCookieName(), Value: id, Path: s.cfg.BasePath + "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 60 * 60})
	s.sessionReply(w, current)
}

func (s *Server) sessionReply(w http.ResponseWriter, current session) {
	reply(w, http.StatusOK, map[string]any{
		"user":           map[string]string{"id": current.ownerID, "login": current.edgeUser, "name": current.edgeUser},
		"csrf":           current.csrf,
		"writes_enabled": s.cfg.HarnessCommands,
	})
}

func (s *Server) sessionCookieName() string {
	if s.cfg.BasePath != "" {
		return "__Secure-panel_session"
	}
	return cookieName
}

func applyNotFound(w http.ResponseWriter) {
	reply(w, http.StatusNotFound, map[string]string{"error": "not_found"})
}
