package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
)

// Internal links are written as relative markdown file links, e.g.
// `[Builtins](../shell/help-builtins.md#files-in-tmp)`, so they also work
// in an editor or on GitHub. docgen resolves each one against the loaded
// docs: on the web it becomes /manual/<section>#<anchor>; in the shell it
// becomes the label plus the target's first shell topic, "(man alert)".
// A link to a missing doc or heading fails the build.

// links is the index for the current run; main sets it once every doc is
// loaded. Nil (as in most tests) leaves links untouched.
var links *linkIndex

// linkIndex knows every doc by source path and the heading ids each one
// renders, so a link can be checked and rewritten.
type linkIndex struct {
	byPath map[string]*doc
	ids    map[string]map[string]bool
	errs   []string
}

func newLinkIndex(docs []*doc) *linkIndex {
	idx := &linkIndex{byPath: map[string]*doc{}, ids: map[string]map[string]bool{}}
	for _, d := range docs {
		if d.path == "" {
			continue
		}
		p := filepath.Clean(d.path)
		idx.byPath[p] = d
		idx.ids[p] = headingIDs(d.body)
	}
	return idx
}

func headingIDs(body []byte) map[string]bool {
	md := goldmark.New(
		goldmark.WithExtensions(extension.Table),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
	root := md.Parser().Parse(gmtext.NewReader(body))
	ids := map[string]bool{}
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if h, ok := n.(*ast.Heading); ok && entering {
			if v, ok := h.AttributeString("id"); ok {
				if b, ok := v.([]byte); ok {
					ids[string(b)] = true
				}
			}
		}
		return ast.WalkContinue, nil
	})
	return ids
}

// linkCtx resolves links inside one source doc. A nil *linkCtx leaves
// every link untouched.
type linkCtx struct {
	idx  *linkIndex
	from *doc
}

func linkCtxFor(idx *linkIndex, d *doc) *linkCtx {
	if idx == nil || d == nil || d.path == "" {
		return nil
	}
	return &linkCtx{idx: idx, from: d}
}

func (lc *linkCtx) fail(format string, args ...any) {
	lc.idx.errs = append(lc.idx.errs, fmt.Sprintf("%s: %s", lc.from.path, fmt.Sprintf(format, args...)))
}

// resolve returns the target doc and fragment for an internal link, or
// ok=false when dest is external, a bare #anchor, or invalid (invalid
// ones are recorded as errors).
func (lc *linkCtx) resolve(dest string) (target *doc, frag string, ok bool) {
	if dest == "" || strings.HasPrefix(dest, "#") || strings.Contains(dest, ":") {
		return nil, "", false
	}
	p, frag, _ := strings.Cut(dest, "#")
	key := filepath.Clean(filepath.Join(filepath.Dir(lc.from.path), p))
	target = lc.idx.byPath[key]
	if target == nil {
		lc.fail("link %q does not point at a doc", dest)
		return nil, "", false
	}
	if frag != "" && !lc.idx.ids[key][frag] {
		lc.fail("link %q: no heading #%s in %s", dest, frag, key)
		return nil, "", false
	}
	return target, frag, true
}

// rewriteWeb points internal links at /manual/<section>. A link to a doc
// with no web page (shell-only) is dropped to its plain label.
func (lc *linkCtx) rewriteWeb(root ast.Node) {
	for _, l := range collectLinks(root) {
		target, frag, ok := lc.resolve(string(l.Destination))
		if !ok {
			continue
		}
		if target.Web == nil {
			unlink(l)
			continue
		}
		href := "/manual/" + target.Web.Section
		if frag != "" {
			href += "#" + frag
		}
		l.Destination = []byte(href)
	}
}

// rewriteShell turns internal links into "label (man topic)", or just the
// label when the target has no shell topic.
func (lc *linkCtx) rewriteShell(root ast.Node) {
	for _, l := range collectLinks(root) {
		dest := string(l.Destination)
		target, _, ok := lc.resolve(dest)
		if !ok && !strings.HasPrefix(dest, "#") {
			continue
		}
		var label strings.Builder
		for c := l.FirstChild(); c != nil; c = c.NextSibling() {
			walkInline(c, lc.from.body, &label)
		}
		text := label.String()
		if ok && target.Shell != nil && len(target.Shell.Topics) > 0 {
			text += " (" + target.Shell.Topics[0] + ")"
		}
		l.Parent().ReplaceChild(l.Parent(), l, ast.NewString([]byte(text)))
	}
}

func collectLinks(root ast.Node) []*ast.Link {
	var out []*ast.Link
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if l, ok := n.(*ast.Link); ok && entering {
			out = append(out, l)
		}
		return ast.WalkContinue, nil
	})
	return out
}

// unlink replaces a link node with its children.
func unlink(l *ast.Link) {
	parent := l.Parent()
	for c := l.FirstChild(); c != nil; {
		next := c.NextSibling()
		parent.InsertBefore(parent, l, c)
		c = next
	}
	parent.RemoveChild(parent, l)
}
