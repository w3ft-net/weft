package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
)

// A doc pulls a section of another doc into its own body with a directive on
// a line by itself:
//
//	{{include ../manual/alerts.md#conditions}}
//
// The path is relative to the including doc, and the fragment is the heading
// id a link to that heading would use. The text under that heading (up to the
// next heading of the same or a higher level, sub-headings included) replaces
// the directive; the heading line itself does not, so the includer chooses
// its own heading. The section is written once and both pages render it.
var includeRE = regexp.MustCompile(`(?m)^\{\{include ([^#\s}]+)#([^\s}]+)\}\}[ \t]*$`)

// linkDestRE finds inline link destinations, so links in an included section
// can be re-pointed relative to the including doc.
var linkDestRE = regexp.MustCompile(`\]\(([^)\s]*)\)`)

// resolveIncludes expands every include directive in docs' bodies. It runs
// after all docs are loaded and before the link index is built, so included
// text is checked and rendered like any other.
func resolveIncludes(docs []*doc) error {
	byPath := map[string]*doc{}
	raw := map[string][]byte{}
	for _, d := range docs {
		if d.path == "" {
			continue
		}
		p := filepath.Clean(d.path)
		byPath[p] = d
		raw[p] = d.body
	}
	done := map[string][]byte{}
	var expand func(p string, stack []string) ([]byte, error)
	expand = func(p string, stack []string) ([]byte, error) {
		if b, ok := done[p]; ok {
			return b, nil
		}
		for _, s := range stack {
			if s == p {
				return nil, fmt.Errorf("include cycle: %s", strings.Join(append(stack, p), " -> "))
			}
		}
		var firstErr error
		out := includeRE.ReplaceAllFunc(raw[p], func(m []byte) []byte {
			sub := includeRE.FindSubmatch(m)
			src := filepath.Clean(filepath.Join(filepath.Dir(p), string(sub[1])))
			if _, ok := byPath[src]; !ok {
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: include %q does not point at a doc", p, sub[1])
				}
				return m
			}
			body, err := expand(src, append(stack, p))
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return m
			}
			sec, err := sectionBody(body, string(sub[2]))
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: include %s: %w", p, sub[1], err)
				}
				return m
			}
			return retargetLinks(sec, src, p)
		})
		if firstErr != nil {
			return nil, firstErr
		}
		done[p] = out
		return out, nil
	}
	for p, d := range byPath {
		b, err := expand(p, nil)
		if err != nil {
			return err
		}
		d.body = b
	}
	return nil
}

// sectionBody returns the text under the heading with the given id: from the
// line after the heading to the next heading of the same or a higher level,
// or the end of the body.
func sectionBody(body []byte, id string) ([]byte, error) {
	md := goldmark.New(
		goldmark.WithExtensions(extension.Table),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
	root := md.Parser().Parse(gmtext.NewReader(body))
	type head struct {
		level, line int
		id          string
	}
	var heads []head
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		h, ok := n.(*ast.Heading)
		if !ok || !entering || h.Lines().Len() == 0 {
			return ast.WalkContinue, nil
		}
		hid := ""
		if v, ok := h.AttributeString("id"); ok {
			if b, ok := v.([]byte); ok {
				hid = string(b)
			}
		}
		line := bytes.Count(body[:h.Lines().At(0).Start], []byte("\n"))
		heads = append(heads, head{h.Level, line, hid})
		return ast.WalkContinue, nil
	})
	lines := strings.Split(string(body), "\n")
	for i, h := range heads {
		if h.id != id {
			continue
		}
		end := len(lines)
		for _, next := range heads[i+1:] {
			if next.level <= h.level {
				end = next.line
				break
			}
		}
		return []byte(strings.Trim(strings.Join(lines[h.line+1:end], "\n"), "\n")), nil
	}
	return nil, fmt.Errorf("no heading #%s", id)
}

// retargetLinks rewrites relative link destinations in a section written in
// src so they resolve from dst. A bare #anchor stays pointed at src.
func retargetLinks(sec []byte, src, dst string) []byte {
	return linkDestRE.ReplaceAllFunc(sec, func(m []byte) []byte {
		dest := string(linkDestRE.FindSubmatch(m)[1])
		if dest == "" || strings.Contains(dest, ":") {
			return m
		}
		p, frag, hasFrag := strings.Cut(dest, "#")
		target := src
		if p != "" {
			target = filepath.Join(filepath.Dir(src), p)
		}
		rel, err := filepath.Rel(filepath.Dir(dst), target)
		if err != nil {
			return m
		}
		if hasFrag {
			rel += "#" + frag
		}
		return []byte("](" + rel + ")")
	})
}
