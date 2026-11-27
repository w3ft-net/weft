package mem

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/w3ft-net/weft/backends/tmp"
	"github.com/w3ft-net/weft/shape"
)

// Tmp is a tmp.Store in memory: a flat map of normalized path to entry.
type Tmp struct {
	mu    sync.Mutex
	files map[string]*tmpEntry
}

type tmpEntry struct {
	tmp.File
	content []byte
}

var _ tmp.Store = (*Tmp)(nil)

// NewTmp is an empty store.
func NewTmp() *Tmp { return &Tmp{files: map[string]*tmpEntry{}} }

// Limits is none: nobody is held to a quota.
func (s *Tmp) Limits(shape.Added) tmp.Limits { return tmp.Limits{} }

func (s *Tmp) get(path string) (*tmpEntry, error) {
	e, ok := s.files[path]
	if !ok {
		return nil, tmp.ErrNotFound
	}
	return e, nil
}

// under reports whether p is strictly below dir.
func under(p, dir string) bool { return strings.HasPrefix(p, dir+"/") }

func (s *Tmp) ensureParents(path string) error {
	for _, p := range tmp.Parents(path) {
		d, ok := s.files[p]
		if !ok {
			return fmt.Errorf("%w: parent %q does not exist", tmp.ErrNotFound, p)
		}
		if !d.IsDir {
			return fmt.Errorf("%w: %q is not a directory", tmp.ErrNotDir, p)
		}
	}
	return nil
}

func (s *Tmp) usage() int64 {
	var n int64
	for _, e := range s.files {
		if !e.IsDir {
			n += e.Size
		}
	}
	return n
}

func (s *Tmp) check(lim tmp.Limits, size, delta int64) error {
	if lim.MaxFileBytes > 0 && size > lim.MaxFileBytes {
		return fmt.Errorf("%w (%d bytes > %d bytes)", tmp.ErrFileTooLarge, size, lim.MaxFileBytes)
	}
	if lim.QuotaBytes > 0 && delta > 0 {
		if used := s.usage(); used+delta > lim.QuotaBytes {
			return fmt.Errorf("%w (%d + %d > %d)", tmp.ErrQuotaExceeded, used, delta, lim.QuotaBytes)
		}
	}
	return nil
}

func (e *tmpEntry) file() *tmp.File {
	f := e.File
	return &f
}

func (s *Tmp) Stat(_ shape.Added, path string) (*tmp.File, error) {
	p, err := tmp.NormalizePath(path)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.get(p)
	if err != nil {
		return nil, err
	}
	return e.file(), nil
}

func (s *Tmp) List(_ shape.Added, dir string) ([]tmp.File, error) {
	d, err := tmp.NormalizeDir(dir)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []tmp.File
	for p, e := range s.files {
		rest := p
		if d != "" {
			if !under(p, d) {
				continue
			}
			rest = strings.TrimPrefix(p, d+"/")
		}
		if !strings.Contains(rest, "/") {
			out = append(out, e.File)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (s *Tmp) Read(_ shape.Added, path string) ([]byte, error) {
	p, err := tmp.NormalizePath(path)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.get(p)
	if err != nil {
		return nil, err
	}
	if e.IsDir {
		return nil, tmp.ErrIsDir
	}
	return append([]byte{}, e.content...), nil
}

func (s *Tmp) put(lim tmp.Limits, by, path string, data []byte, appendTo bool) (bool, error) {
	p, err := tmp.NormalizePath(path)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureParents(p); err != nil {
		return false, err
	}
	e, ok := s.files[p]
	if ok && e.IsDir {
		return false, tmp.ErrIsDir
	}
	var oldSize int64
	if ok {
		oldSize = e.Size
	}
	newSize, delta := int64(len(data)), int64(len(data))-oldSize
	if appendTo {
		newSize, delta = oldSize+int64(len(data)), int64(len(data))
	}
	if err := s.check(lim, newSize, delta); err != nil {
		return false, err
	}
	now := time.Now().UTC()
	if !ok {
		s.files[p] = &tmpEntry{File: tmp.File{Path: p, Size: newSize, CreatedBy: by, CreatedAt: now, UpdatedAt: now}, content: append([]byte{}, data...)}
		return true, nil
	}
	if appendTo {
		e.content = append(e.content, data...)
	} else {
		e.content = append([]byte{}, data...)
	}
	e.Size, e.UpdatedAt = newSize, now
	return false, nil
}

func (s *Tmp) Write(_ shape.Added, lim tmp.Limits, by, path string, data []byte) (bool, error) {
	return s.put(lim, by, path, data, false)
}

func (s *Tmp) Append(_ shape.Added, lim tmp.Limits, by, path string, data []byte) (bool, error) {
	return s.put(lim, by, path, data, true)
}

func (s *Tmp) Touch(_ shape.Added, _ tmp.Limits, by, path string) (bool, error) {
	p, err := tmp.NormalizePath(path)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if e, ok := s.files[p]; ok {
		e.UpdatedAt = now
		return false, nil
	}
	if err := s.ensureParents(p); err != nil {
		return false, err
	}
	s.files[p] = &tmpEntry{File: tmp.File{Path: p, CreatedBy: by, CreatedAt: now, UpdatedAt: now}}
	return true, nil
}

func (s *Tmp) Mkdir(_ shape.Added, by, path string) error {
	p, err := tmp.NormalizePath(path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureParents(p); err != nil {
		return err
	}
	if _, ok := s.files[p]; ok {
		return tmp.ErrExists
	}
	now := time.Now().UTC()
	s.files[p] = &tmpEntry{File: tmp.File{Path: p, IsDir: true, CreatedBy: by, CreatedAt: now, UpdatedAt: now}}
	return nil
}

func (s *Tmp) Copy(_ shape.Added, lim tmp.Limits, by, src, dst string) error {
	from, err := tmp.NormalizePath(src)
	if err != nil {
		return err
	}
	to, err := tmp.NormalizePath(dst)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.get(from)
	if err != nil {
		return err
	}
	if f.IsDir {
		return fmt.Errorf("%w: cannot copy a directory", tmp.ErrIsDir)
	}
	if _, ok := s.files[to]; ok {
		return tmp.ErrExists
	}
	if err := s.ensureParents(to); err != nil {
		return err
	}
	if err := s.check(lim, f.Size, f.Size); err != nil {
		return err
	}
	now := time.Now().UTC()
	s.files[to] = &tmpEntry{File: tmp.File{Path: to, Size: f.Size, CreatedBy: by, CreatedAt: now, UpdatedAt: now}, content: append([]byte{}, f.content...)}
	return nil
}

func (s *Tmp) Rename(_ shape.Added, src, dst string) error {
	from, err := tmp.NormalizePath(src)
	if err != nil {
		return err
	}
	to, err := tmp.NormalizePath(dst)
	if err != nil {
		return err
	}
	if strings.HasPrefix(to, from+"/") {
		return fmt.Errorf("%w: cannot move %q into itself", tmp.ErrInvalidPath, from)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.get(from)
	if err != nil {
		return err
	}
	if _, ok := s.files[to]; ok {
		return tmp.ErrExists
	}
	if err := s.ensureParents(to); err != nil {
		return err
	}
	now := time.Now().UTC()
	delete(s.files, from)
	f.Path, f.UpdatedAt = to, now
	s.files[to] = f
	if f.IsDir {
		// Everything under a directory moves with it, keeping its own times.
		for p, e := range s.files {
			if under(p, from) {
				delete(s.files, p)
				e.Path = to + strings.TrimPrefix(p, from)
				s.files[e.Path] = e
			}
		}
	}
	return nil
}

func (s *Tmp) Remove(_ shape.Added, path string) error {
	p, err := tmp.NormalizePath(path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.get(p)
	if err != nil {
		return err
	}
	if f.IsDir {
		for q := range s.files {
			if under(q, p) {
				return fmt.Errorf("%w: %s", tmp.ErrNotEmpty, p)
			}
		}
	}
	delete(s.files, p)
	return nil
}

func (s *Tmp) RemoveAll(_ shape.Added, path string) error {
	p, err := tmp.NormalizePath(path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for q := range s.files {
		if q == p || under(q, p) {
			delete(s.files, q)
			n++
		}
	}
	if n == 0 {
		return tmp.ErrNotFound
	}
	return nil
}

func (s *Tmp) UsageBytes(shape.Added) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage(), nil
}

func (s *Tmp) CountByUser(_ shape.Added, by string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, e := range s.files {
		if !e.IsDir && e.CreatedBy == by {
			n++
		}
	}
	return n, nil
}
