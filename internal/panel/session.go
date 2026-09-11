package panel

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"
)

const cookieName = "__Host-panel_session"

type session struct {
	ownerID, edgeUser, csrf string
	created, last           time.Time
}

type sessions struct {
	mu      sync.Mutex
	entries map[[32]byte]session
	now     func() time.Time
}

func randomToken() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes[:]), nil
}

func digest(value string) [32]byte { return sha256.Sum256([]byte(value)) }
func newSessions() *sessions       { return &sessions{entries: make(map[[32]byte]session), now: time.Now} }

func (s *sessions) prune() {
	for key, value := range s.entries {
		if s.now().Sub(value.last) >= 30*time.Minute || s.now().Sub(value.created) >= 8*time.Hour {
			delete(s.entries, key)
		}
	}
}

func (s *sessions) create(ownerID, edgeUser string) (string, session, error) {
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
		for key, value := range s.entries {
			if at.IsZero() || value.last.Before(at) {
				oldest = key
				at = value.last
			}
		}
		delete(s.entries, oldest)
	}
	now := s.now()
	value := session{ownerID: ownerID, edgeUser: edgeUser, csrf: csrf, created: now, last: now}
	s.entries[digest(id)] = value
	return id, value, nil
}

func (s *sessions) get(id string) (session, bool) {
	return s.lookup(id, true)
}

// Stream and polling reads must observe revocation without extending idle TTL.
func (s *sessions) peek(id string) (session, bool) {
	return s.lookup(id, false)
}

func (s *sessions) lookup(id string, touch bool) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	if len(id) != 43 {
		return session{}, false
	}
	value, ok := s.entries[digest(id)]
	if ok && touch {
		value.last = s.now()
		s.entries[digest(id)] = value
	}
	return value, ok
}

func (s *sessions) revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, digest(id))
}

func (s *sessions) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.entries)
}

func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
