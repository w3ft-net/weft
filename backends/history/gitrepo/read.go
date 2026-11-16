package gitrepo

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/gitcore"
	"github.com/w3ft-net/weft/shape"
)

// Repository is the repository as the shell's `git` reads it.
func (s *Store) Repository(shape.Added) (gitcore.Repository, error) { return reader{s}, nil }

type reader struct{ s *Store }

var _ gitcore.Repository = reader{}

// A commit is one record: id, parents, author name, email, unix time, message.
const commitFormat = "--format=%H%x00%P%x00%an%x00%ae%x00%at%x00%B%x01"

func (r reader) Head() (string, gitcore.ObjectID, error) {
	ctx := context.Background()
	ref := ""
	if out, err := r.s.git(ctx, nil, nil, "symbolic-ref", "-q", "--short", "HEAD"); err == nil {
		ref = strings.TrimSpace(string(out))
	}
	out, err := r.s.git(ctx, nil, nil, "rev-parse", "-q", "--verify", "HEAD")
	if err != nil {
		return ref, "", nil // an unborn branch has no commit yet
	}
	return ref, strings.TrimSpace(string(out)), nil
}

func (r reader) Refs() (map[string]gitcore.ObjectID, error) {
	out, err := r.s.git(context.Background(), nil, nil, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(*objectname)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, err
	}
	refs := map[string]gitcore.ObjectID{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\x00")
		if len(f) != 3 {
			continue
		}
		refs[f[0]] = f[1]
		if f[2] != "" { // an annotated tag names the commit it points at
			refs[f[0]] = f[2]
		}
	}
	return refs, nil
}

func parseCommits(out []byte) ([]*gitcore.Commit, error) {
	var commits []*gitcore.Commit
	for _, rec := range strings.Split(string(out), "\x01") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x00", 6)
		if len(f) != 6 {
			return nil, fmt.Errorf("gitrepo: unreadable commit record %q", rec)
		}
		at, err := strconv.ParseInt(f[4], 10, 64)
		if err != nil {
			return nil, err
		}
		c := &gitcore.Commit{
			ID: f[0], Author: f[2], Email: f[3],
			Time: time.Unix(at, 0), Message: strings.TrimSpace(f[5]),
		}
		if f[1] != "" {
			c.Parents = strings.Fields(f[1])
		}
		commits = append(commits, c)
	}
	return commits, nil
}

func (r reader) Commits() ([]*gitcore.Commit, error) {
	out, err := r.s.git(context.Background(), nil, nil, "log", "--branches", "--tags", commitFormat)
	if err != nil {
		return nil, err
	}
	return parseCommits(out)
}

func (r reader) Commit(id gitcore.ObjectID) (*gitcore.Commit, error) {
	ctx := context.Background()
	full, err := r.s.git(ctx, nil, nil, "rev-parse", "-q", "--verify", id+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("no such commit %q", id)
	}
	out, err := r.s.git(ctx, nil, nil, "log", "-1", commitFormat, strings.TrimSpace(string(full)))
	if err != nil {
		return nil, err
	}
	cs, err := parseCommits(out)
	if err != nil || len(cs) != 1 {
		return nil, fmt.Errorf("no such commit %q", id)
	}
	return cs[0], nil
}

// Tree is the files at the commit. A commit never changes, so what is read is
// kept.
func (r reader) Tree(id gitcore.ObjectID) (map[string]string, error) {
	r.s.treesLock.Lock()
	if t, ok := r.s.trees[id]; ok {
		r.s.treesLock.Unlock()
		return t, nil
	}
	r.s.treesLock.Unlock()

	ctx := context.Background()
	list, err := r.s.git(ctx, nil, nil, "ls-tree", "-r", "-z", "--full-tree", id)
	if err != nil {
		return nil, fmt.Errorf("no such commit %q", id)
	}
	var shas, paths []string
	for _, ent := range strings.Split(string(list), "\x00") {
		meta, path, ok := strings.Cut(ent, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta) // mode, type, sha
		if len(f) == 3 && f[1] == "blob" {
			shas, paths = append(shas, f[2]), append(paths, path)
		}
	}
	tree := make(map[string]string, len(paths))
	if len(shas) > 0 {
		blobs, err := r.s.git(ctx, nil, []byte(strings.Join(shas, "\n")+"\n"), "cat-file", "--batch")
		if err != nil {
			return nil, err
		}
		for i := range shas {
			nl := bytes.IndexByte(blobs, '\n')
			if nl < 0 {
				return nil, fmt.Errorf("gitrepo: short cat-file output")
			}
			f := strings.Fields(string(blobs[:nl])) // sha, type, size
			size, err := strconv.Atoi(f[len(f)-1])
			if err != nil || nl+1+size > len(blobs) {
				return nil, fmt.Errorf("gitrepo: unreadable cat-file header %q", blobs[:nl])
			}
			tree[paths[i]] = string(blobs[nl+1 : nl+1+size])
			blobs = blobs[nl+1+size+1:] // the content, then a newline
		}
	}
	r.s.treesLock.Lock()
	r.s.trees[id] = tree
	r.s.treesLock.Unlock()
	return tree, nil
}
