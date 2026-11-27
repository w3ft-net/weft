package mem

import (
	"sort"
	"sync"
	"time"

	"github.com/w3ft-net/weft/backends/notify"
	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/shape"
)

// Notify is the notify.yaml document and the secrets, in memory. Secrets are
// held in clear: nothing here is at rest.
type Notify struct {
	mu      sync.Mutex
	set     bool
	text    string
	message string
	author  string
	updated time.Time
	secrets map[string]string
}

var (
	_ notify.Store   = (*Notify)(nil)
	_ notify.Secrets = (*Notify)(nil)
)

// NewNotify is an empty store.
func NewNotify() *Notify { return &Notify{secrets: map[string]string{}} }

// Service is the notifycfg.Service over this store.
func (s *Notify) Service() notifycfg.Service { return notifycfg.Service{Aliases: s, Secrets: s} }

func (s *Notify) Aliases(shape.Added) (notify.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.set {
		return notify.Document{}, nil
	}
	return notify.Document{Text: s.text, UpdatedAt: s.updated}, nil
}

func (s *Notify) SaveAliases(_ shape.Added, user, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set && s.text == text {
		return nil
	}
	s.set, s.text, s.message, s.author, s.updated = true, text, "", user, time.Now().UTC()
	return nil
}

func (s *Notify) AmendMessage(_ shape.Added, authorID, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.set {
		return notify.ErrNotFound
	}
	if authorID != "" && s.author != authorID {
		return notify.ErrNotYours
	}
	s.message = message
	return nil
}

func (s *Notify) Names(shape.Added) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := []string{}
	for n := range s.secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

func (s *Notify) Value(_ shape.Added, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.secrets[name]
	if !ok {
		return "", notify.ErrNotFound
	}
	return v, nil
}

func (s *Notify) Set(_ shape.Added, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets[name] = value
	return nil
}

func (s *Notify) Delete(_ shape.Added, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.secrets[name]; !ok {
		return notify.ErrNotFound
	}
	delete(s.secrets, name)
	return nil
}
