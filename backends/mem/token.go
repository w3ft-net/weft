package mem

import (
	"fmt"
	"sync"
	"time"

	"github.com/w3ft-net/weft/backends/token"
	"github.com/w3ft-net/weft/shape"
)

// Tokens is a token.Store in memory over the namespaces of a Sources: a token
// names the namespace it is for, and deleting the namespace deletes the tokens.
type Tokens struct {
	mu     sync.Mutex
	src    *Sources
	nextID uint
	rows   []*tokenRow
}

type tokenRow struct {
	token.Token
	hash string
}

var _ token.Store = (*Tokens)(nil)

// NewTokens is an empty store over src's namespaces.
func NewTokens(src *Sources) *Tokens {
	t := &Tokens{src: src}
	src.mu.Lock()
	src.onDeleteNamespace = append(src.onDeleteNamespace, t.dropNamespace)
	src.mu.Unlock()
	return t
}

func (s *Tokens) dropNamespace(id uint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.rows[:0]
	for _, r := range s.rows {
		if r.Namespace.ID != id {
			kept = append(kept, r)
		}
	}
	s.rows = kept
}

func (s *Tokens) CreateToken(_ shape.Added, namespaceID uint, label, env string) (string, *token.Token, error) {
	raw, err := token.NewRaw()
	if err != nil {
		return "", nil, err
	}
	publicID, err := token.NewPublicID()
	if err != nil {
		return "", nil, err
	}
	name, ok, _ := s.src.NamespaceName(shape.Added{}, namespaceID)
	if !ok {
		return "", nil, fmt.Errorf("namespace %d does not exist", namespaceID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	r := &tokenRow{hash: token.Hash(raw), Token: token.Token{ID: s.nextID, PublicID: publicID, Prefix: token.Prefix(raw),
		Namespace: token.NamespaceRef{ID: namespaceID, Name: name}, Label: label, Env: env, CreatedAt: time.Now().UTC()}}
	s.rows = append(s.rows, r)
	t := r.Token
	return raw, &t, nil
}

func (s *Tokens) ValidateToken(raw string) (*token.Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := token.Hash(raw)
	for _, r := range s.rows {
		if r.hash == h && !r.Revoked {
			return &token.Auth{TokenID: r.ID, Namespace: r.Namespace.Name, Env: r.Env}, nil
		}
	}
	return nil, nil
}

func (s *Tokens) RevokeToken(_ shape.Added, publicID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.PublicID == publicID {
			r.Revoked = true
			return nil
		}
	}
	return token.ErrNotFound
}

func (s *Tokens) ListTokens(shape.Added) ([]token.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []token.Token
	for _, r := range s.rows {
		t := r.Token
		t.MetricsCollectors = append([]string(nil), r.MetricsCollectors...)
		out = append(out, t)
	}
	return out, nil
}

func (s *Tokens) CountLiveTokens(_ shape.Added, namespaceID uint) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.rows {
		if r.Namespace.ID == namespaceID && !r.Revoked {
			n++
		}
	}
	return n, nil
}

func (s *Tokens) SetMetricsConfig(raw string, cfg token.MetricsConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := token.Hash(raw)
	for _, r := range s.rows {
		if r.hash == h {
			r.MetricsEnabled, r.MetricsInterval, r.MetricsCollectors = cfg.Enabled, cfg.Interval, append([]string(nil), cfg.Collectors...)
		}
	}
	return nil
}
