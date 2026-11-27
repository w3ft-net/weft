package mem

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/w3ft-net/weft/backends/notice"
)

// Notices is a notice.Store in memory.
type Notices struct {
	mu     sync.Mutex
	nextID int64
	rows   []noticeRow
}

type noticeRow struct {
	user    string
	pending notice.Pending
	seen    bool
}

var _ notice.Store = (*Notices)(nil)

// NewNotices is an empty store.
func NewNotices() *Notices { return &Notices{} }

func (s *Notices) Emit(userID, key, kind string, params map[string]any) error {
	switch kind {
	case notice.KindSuccess, notice.KindWarning, notice.KindError:
	default:
		return fmt.Errorf("%w %q (want success/warning/error)", notice.ErrInvalidKind, kind)
	}
	// Through JSON, as a persistent store would return them: lists are []any and
	// numbers float64.
	var decoded map[string]any
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("encode notice params: %w", err)
		}
		if err := json.Unmarshal(b, &decoded); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	s.rows = append(s.rows, noticeRow{user: userID, pending: notice.Pending{ID: s.nextID, Key: key, Kind: kind, Params: decoded, CreatedAt: time.Now().UTC()}})
	return nil
}

func (s *Notices) Pending(userID string) ([]notice.Pending, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []notice.Pending{}
	for _, r := range s.rows {
		if r.user == userID && !r.seen {
			out = append(out, r.pending)
		}
	}
	return out, nil
}

func (s *Notices) MarkSeen(ids []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		for i := range s.rows {
			if s.rows[i].pending.ID == id {
				s.rows[i].seen = true
			}
		}
	}
	return nil
}
