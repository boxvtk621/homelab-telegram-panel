package panel

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/youtrack"
)

const cookieName = "__Host-panel_session"

type permit struct {
	target  string
	expires time.Time
}
type session struct {
	user          youtrack.User
	token, csrf   string
	created, last time.Time
	permits       map[string]permit
}
type sessions struct {
	mu      sync.Mutex
	entries map[[32]byte]session
	now     func() time.Time
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
func digest(s string) [32]byte { return sha256.Sum256([]byte(s)) }
func newSessions() *sessions   { return &sessions{entries: make(map[[32]byte]session), now: time.Now} }
func (s *sessions) prune() {
	for k, v := range s.entries {
		if s.now().Sub(v.last) >= 30*time.Minute || s.now().Sub(v.created) >= 8*time.Hour {
			delete(s.entries, k)
		}
	}
}
func (s *sessions) create(u youtrack.User, token string) (string, session, error) {
	id, err := randomToken()
	if err != nil {
		return "", session{}, err
	}
	csrf, err := randomToken()
	if err != nil {
		return "", session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	if len(s.entries) >= 4 {
		var oldest [32]byte
		var at time.Time
		for k, v := range s.entries {
			if at.IsZero() || v.last.Before(at) {
				oldest = k
				at = v.last
			}
		}
		delete(s.entries, oldest)
	}
	v := session{user: u, token: token, csrf: csrf, created: s.now(), last: s.now(), permits: make(map[string]permit)}
	s.entries[digest(id)] = v
	return id, v, nil
}
func (s *sessions) get(id string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	if len(id) != 43 {
		return session{}, false
	}
	v, ok := s.entries[digest(id)]
	if ok {
		v.last = s.now()
		s.entries[digest(id)] = v
	}
	v.permits = nil
	return v, ok
}
func (s *sessions) revoke(id string) { s.mu.Lock(); defer s.mu.Unlock(); delete(s.entries, digest(id)) }
func (s *sessions) close()           { s.mu.Lock(); defer s.mu.Unlock(); clear(s.entries) }
func equal(a, b string) bool         { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func (s *sessions) issuePermit(id, target string) (string, error) {
	key, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	v, ok := s.entries[digest(id)]
	if !ok {
		return "", youtrack.ErrDenied
	}
	for k, p := range v.permits {
		if !s.now().Before(p.expires) {
			delete(v.permits, k)
		}
	}
	if len(v.permits) >= 8 {
		return "", youtrack.ErrUnavailable
	}
	v.permits[key] = permit{target: target, expires: s.now().Add(5 * time.Minute)}
	return key, nil
}
func (s *sessions) consume(id, key, target string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	v, ok := s.entries[digest(id)]
	if !ok {
		return false
	}
	p, ok := v.permits[key]
	if !ok || p.target != target {
		return false
	}
	delete(v.permits, key)
	return s.now().Before(p.expires)
}
