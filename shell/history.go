package shell

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/kevlarcon/warp/pkg/shellcore"
)

// FileHistoryStore persists history to ~/.weft_history.
type FileHistoryStore struct {
	path string
}

// NewFileHistoryStore creates a store writing to ~/.weft_history.
func NewFileHistoryStore() *FileHistoryStore {
	home, _ := os.UserHomeDir()
	return &FileHistoryStore{path: filepath.Join(home, ".weft_history")}
}

func (s *FileHistoryStore) Load() ([]string, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	// Filter empty lines.
	var entries []string
	for _, line := range lines {
		if line != "" {
			entries = append(entries, line)
		}
	}
	return entries, nil
}

func (s *FileHistoryStore) Save(entries []string) error {
	tmp := s.path + ".tmp"
	data := strings.Join(entries, "\n") + "\n"
	if err := os.WriteFile(tmp, []byte(data), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Verify interface compliance.
var _ shellcore.HistoryStore = (*FileHistoryStore)(nil)
