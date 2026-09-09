package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/cursoragent"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
	"github.com/boxvtk621/homelab-telegram-panel/internal/youtrack"
)

type Server struct {
	cfg              Config
	client           *youtrack.Client
	static           http.Handler
	sessions         *sessions
	general, auth    chan struct{}
	mu               sync.Mutex
	loginWindow      time.Time
	loginAttempts    int
	agent            cursoragent.Executor
	runs             *agentRuns
	harness          *harnessclient.Client
	streams, control chan struct{}
	commandBodies    chan struct{}
}

func New(cfg Config, static http.Handler) (*Server, error) {
	client, err := youtrack.New(cfg.YouTrackURL, cfg.ProjectID, cfg.ProjectKey)
	if err != nil {
		return nil, err
	}
	if static == nil {
		client.Close()
		return nil, errors.New("missing web assets")
	}
	s := &Server{cfg: cfg, client: client, static: static, sessions: newSessions(), general: make(chan struct{}, 8), auth: make(chan struct{}, 2), runs: newAgentRuns()}
	s.harness, err = harnessclient.Load(cfg.Harness)
	if err != nil {
		client.Close()
		return nil, err
	}
	s.streams, s.control = make(chan struct{}, 4), make(chan struct{}, 2)
	s.commandBodies = make(chan struct{}, 4)
	if cfg.CursorKey != "" {
		s.agent = cursoragent.Runner{Config: cursoragent.Config{Python: cfg.CursorPython, Worker: cfg.CursorWorker, Model: cfg.CursorModel, Key: cfg.CursorKey}}
	}
	s.runs.pruneHistory()
	return s, nil
}
func (s *Server) Close() {
	if s.harness != nil {
		s.harness.Close()
	}
	if s.runs != nil {
		s.runs.close()
	}
	s.sessions.close()
	s.client.Close()
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, err error) {
	status := 503
	code := "youtrack_unavailable"
	switch {
	case errors.Is(err, youtrack.ErrDenied):
		status = 403
		code = "access_denied"
	case errors.Is(err, youtrack.ErrInvalid):
		status = 400
		code = "invalid_request"
	case errors.Is(err, youtrack.ErrUnknown):
		status = 409
		code = "write_outcome_unknown"
	}
	reply(w, status, map[string]string{"error": code})
}
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || ct != "application/json" {
		failure(w, youtrack.ErrInvalid)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 72<<10)
	raw, err := io.ReadAll(r.Body)
	if err != nil || !strictjson.Valid(raw) {
		failure(w, youtrack.ErrInvalid)
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil || d.Decode(new(any)) != io.EOF {
		failure(w, youtrack.ErrInvalid)
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
		failure(w, youtrack.ErrDenied)
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
		reply(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	if r.Method == http.MethodGet && (r.ContentLength != 0 || len(r.TransferEncoding) != 0) {
		failure(w, youtrack.ErrInvalid)
		return
	}
	if r.Method == http.MethodPost && (len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != s.cfg.Origin || r.URL.RawQuery != "") {
		failure(w, youtrack.ErrDenied)
		return
	}
	if r.URL.Path == "/api/v2/healthz" && r.Method == http.MethodGet {
		reply(w, 200, map[string]string{"panel": "up"})
		return
	}
	gate := s.general
	if r.URL.Path == "/api/v2/login" {
		gate = s.auth
	}
	harnessRoute := strings.HasPrefix(r.URL.Path, "/api/v2/harness/")
	// Revocation is a bounded local operation. Remote work must never occupy
	// the last slot needed to revoke a session and close its streams.
	logout := r.URL.Path == "/api/v2/logout" && r.Method == http.MethodPost
	if !harnessRoute && !logout {
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		default:
			reply(w, 429, map[string]string{"error": "capacity_exhausted"})
			return
		}
	}
	if r.URL.Path == "/api/v2/login" && r.Method == http.MethodPost {
		s.login(w, r)
		return
	}
	cookies := r.CookiesNamed(s.sessionCookieName())
	if len(cookies) != 1 {
		reply(w, 401, map[string]string{"error": "authentication_required"})
		return
	}
	id := cookies[0].Value
	var sess session
	var ok bool
	if harnessRoute && r.Method == http.MethodGet {
		sess, ok = s.sessions.peek(id)
	} else {
		sess, ok = s.sessions.get(id)
	}
	if !ok {
		reply(w, 401, map[string]string{"error": "authentication_required"})
		return
	}
	if r.Method == http.MethodPost && (len(r.Header.Values("X-Panel-CSRF")) != 1 || !equal(r.Header.Get("X-Panel-CSRF"), sess.csrf)) {
		failure(w, youtrack.ErrDenied)
		return
	}
	if harnessRoute {
		s.harnessHTTP(w, r, id, sess)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v2/agent/") {
		s.agentHTTP(w, r, sess)
		return
	}
	if r.Method == http.MethodPost {
		s.write(w, r, id, sess)
		return
	}
	s.read(w, r, sess)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if time.Since(s.loginWindow) >= time.Minute {
		s.loginWindow = time.Now()
		s.loginAttempts = 0
	}
	allowed := s.loginAttempts < 12
	s.loginAttempts++
	s.mu.Unlock()
	if !allowed {
		reply(w, 429, map[string]string{"error": "login_rate_limited"})
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &in) {
		return
	}
	u, err := s.client.Me(r.Context(), in.Token)
	if err != nil {
		failure(w, err)
		return
	}
	if u.Login != s.cfg.OwnerLogin {
		failure(w, youtrack.ErrDenied)
		return
	}
	id, sess, err := s.sessions.create(u, in.Token)
	if err != nil {
		failure(w, youtrack.ErrUnavailable)
		return
	}
	for _, c := range r.CookiesNamed(s.sessionCookieName()) {
		s.sessions.revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: s.sessionCookieName(), Value: id, Path: s.cfg.BasePath + "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 60 * 60})
	s.sessionReply(w, sess)
}
func (s *Server) sessionReply(w http.ResponseWriter, v session) {
	reply(w, 200, map[string]any{"user": v.user, "csrf": v.csrf, "writes_enabled": s.cfg.Writes, "youtrack_url": s.client.Origin(), "project": s.cfg.ProjectKey})
}

func (s *Server) sessionCookieName() string {
	if s.cfg.BasePath != "" {
		// __Host- requires Path=/. A path mount uses __Secure- without Domain.
		// Path limits cookie delivery, not same-origin script authority.
		return "__Secure-panel_session"
	}
	return cookieName
}

func (s *Server) read(w http.ResponseWriter, r *http.Request, v session) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		failure(w, youtrack.ErrInvalid)
		return
	}
	for key, values := range q {
		if key != "skip" || len(values) != 1 {
			failure(w, youtrack.ErrInvalid)
			return
		}
	}
	skip := 0
	if q.Has("skip") {
		skip, err = strconv.Atoi(q.Get("skip"))
		if err != nil || skip < 0 || skip > 100000 {
			failure(w, youtrack.ErrInvalid)
			return
		}
	}
	p := strings.TrimPrefix(r.URL.Path, "/api/v2/")
	var out any
	switch {
	case p == "session":
		s.sessionReply(w, v)
		return
	case p == "readyz":
		_, err = s.client.Me(r.Context(), v.token)
		out = map[string]string{"youtrack": "reachable"}
	case p == "issues":
		out, err = s.client.Issues(r.Context(), v.token, skip)
	case p == "articles":
		out, err = s.client.Articles(r.Context(), v.token, skip)
	case strings.HasPrefix(p, "issues/") && strings.HasSuffix(p, "/comments"):
		out, err = s.client.Comments(r.Context(), v.token, strings.TrimSuffix(strings.TrimPrefix(p, "issues/"), "/comments"), skip)
	case strings.HasPrefix(p, "issues/"):
		out, err = s.client.Issue(r.Context(), v.token, strings.TrimPrefix(p, "issues/"))
	case strings.HasPrefix(p, "articles/"):
		out, err = s.client.Article(r.Context(), v.token, strings.TrimPrefix(p, "articles/"))
	default:
		reply(w, 404, map[string]string{"error": "not_found"})
		return
	}
	if err != nil {
		failure(w, err)
		return
	}
	reply(w, 200, map[string]any{"data": out, "observed_at": time.Now().UTC().Format(time.RFC3339)})
}

func (s *Server) write(w http.ResponseWriter, r *http.Request, id string, v session) {
	p := strings.TrimPrefix(r.URL.Path, "/api/v2/")
	if p == "logout" {
		s.sessions.revoke(id)
		http.SetCookie(w, &http.Cookie{Name: s.sessionCookieName(), Value: "", Path: s.cfg.BasePath + "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		reply(w, 200, map[string]bool{"logged_out": true})
		return
	}
	if !s.cfg.Writes {
		failure(w, youtrack.ErrDenied)
		return
	}
	if p == "permits" {
		var in struct {
			Target string `json:"target"`
		}
		if !decode(w, r, &in) {
			return
		}
		issue, ok := strings.CutSuffix(in.Target, "/comments")
		if !ok || !s.client.ValidIssue(issue) {
			failure(w, youtrack.ErrInvalid)
			return
		}
		if _, err := s.client.Issue(r.Context(), v.token, issue); err != nil {
			failure(w, err)
			return
		}
		key, err := s.sessions.issuePermit(id, in.Target)
		if err != nil {
			failure(w, err)
			return
		}
		reply(w, 200, map[string]string{"permit": key})
		return
	}
	if strings.HasPrefix(p, "issues/") && strings.HasSuffix(p, "/comments") {
		target := strings.TrimPrefix(p, "issues/")
		issue := strings.TrimSuffix(target, "/comments")
		var in struct {
			Permit string `json:"permit"`
			Text   string `json:"text"`
		}
		if !decode(w, r, &in) {
			return
		}
		if !s.client.ValidIssue(issue) || strings.TrimSpace(in.Text) == "" || len(in.Text) > 64<<10 {
			failure(w, youtrack.ErrInvalid)
			return
		}
		// A used/unknown/expired permit never causes another upstream POST, including
		// after process restart. This is replay prevention, not a durable command queue.
		if !s.sessions.consume(id, in.Permit, target) {
			failure(w, youtrack.ErrUnknown)
			return
		}
		out, err := s.client.AddComment(r.Context(), v.token, issue, in.Text)
		if err != nil {
			failure(w, err)
			return
		}
		reply(w, 201, map[string]any{"data": out, "recorded_in": "youtrack", "execution_accepted": false})
		return
	}
	reply(w, 404, map[string]string{"error": "not_found"})
}
