package mem

import (
	"sync"
	"time"

	"github.com/w3ft-net/weft/backends/login"
)

// Login is a login.Store in memory.
type Login struct {
	mu      sync.Mutex
	emails  map[string]bool
	links   map[string]*loginLink
	nowFunc func() time.Time
}

type loginLink struct {
	email   string
	expires time.Time
	used    bool
}

var _ login.Store = (*Login)(nil)

// NewLogin is an empty store: nobody may sign in until Allow adds an address.
func NewLogin() *Login {
	return &Login{emails: map[string]bool{}, links: map[string]*loginLink{}, nowFunc: time.Now}
}

// Allow lets an address sign in.
func (s *Login) Allow(email string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emails[login.Normalize(email)] = true
}

func (s *Login) Allowed(email string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emails[login.Normalize(email)], nil
}

func (s *Login) IssueLink(email string, ttl time.Duration) (string, error) {
	raw, err := login.NewRaw()
	if err != nil {
		return "", err
	}
	if ttl < 0 {
		ttl = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.links[login.Hash(raw)] = &loginLink{email: login.Normalize(email), expires: s.nowFunc().Add(ttl)}
	return raw, nil
}

func (s *Login) ConsumeLink(raw string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[login.Hash(raw)]
	if !ok || l.used || !l.expires.After(s.nowFunc()) {
		return "", false, nil
	}
	l.used = true
	return l.email, true, nil
}
