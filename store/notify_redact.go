package store

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// NotifyRedacted is what a webhook header value shows in place of the
// credential when /etc/notify.yaml is read.
const NotifyRedacted = "<redacted>"

// webhookHeader is one `headers:` entry of a webhook target in a parsed
// notify.yaml, with the alias and URL that identify it.
type webhookHeader struct {
	alias, url, key string
	val             *yaml.Node
}

// webhookHeaders lists every webhook header in doc (a parsed notify.yaml):
// alias -> webhook: [ {url, headers: {name: value}} ].
func webhookHeaders(doc *yaml.Node) []webhookHeader {
	var out []webhookHeader
	if doc == nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	top := doc.Content[0]
	for i := 0; i+1 < len(top.Content); i += 2 {
		alias, body := top.Content[i].Value, top.Content[i+1]
		hooks := mappingValue(body, "webhook")
		if hooks == nil || hooks.Kind != yaml.SequenceNode {
			continue
		}
		for _, item := range hooks.Content {
			if item.Kind != yaml.MappingNode {
				continue // a bare URL string has no headers
			}
			url := ""
			if u := mappingValue(item, "url"); u != nil {
				url = u.Value
			}
			hdrs := mappingValue(item, "headers")
			if hdrs == nil || hdrs.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(hdrs.Content); j += 2 {
				out = append(out, webhookHeader{alias: alias, url: url, key: hdrs.Content[j].Value, val: hdrs.Content[j+1]})
			}
		}
	}
	return out
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func encodeNotifyYAML(doc *yaml.Node) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// RedactNotifyYAML returns raw with every webhook header value replaced by
// NotifyRedacted. Header values are credentials (an Authorization token, a
// signing secret); everything else - URLs, header names, channels, comments
// - stays readable. Input with no headers, or that doesn't parse, comes
// back unchanged.
func RedactNotifyYAML(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return raw
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return raw
	}
	hdrs := webhookHeaders(&doc)
	if len(hdrs) == 0 {
		return raw
	}
	for _, h := range hdrs {
		h.val.Kind, h.val.Tag, h.val.Value, h.val.Style = yaml.ScalarNode, "!!str", NotifyRedacted, yaml.DoubleQuotedStyle
	}
	out, err := encodeNotifyYAML(&doc)
	if err != nil {
		return raw
	}
	return out
}

// RestoreNotifyRedactions is the write half: newRaw is what the user saved,
// stored is what's stored now. A header still showing NotifyRedacted gets its
// stored value back (matched by alias, webhook URL and header name); a header
// given a new value keeps it. A placeholder with nothing stored behind it is
// an error, never saved as a literal credential. Text with no placeholders
// is returned exactly as written.
func RestoreNotifyRedactions(newRaw, stored string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(newRaw), &doc); err != nil {
		return newRaw, nil // let ParseNotifyAliases report the syntax error
	}
	var pending []webhookHeader
	for _, h := range webhookHeaders(&doc) {
		if h.val.Value == NotifyRedacted {
			pending = append(pending, h)
		}
	}
	if len(pending) == 0 {
		return newRaw, nil
	}
	have := map[[3]string]string{}
	if strings.TrimSpace(stored) != "" {
		var old yaml.Node
		if err := yaml.Unmarshal([]byte(stored), &old); err == nil {
			for _, h := range webhookHeaders(&old) {
				k := [3]string{h.alias, h.url, h.key}
				if _, dup := have[k]; !dup {
					have[k] = h.val.Value
				}
			}
		}
	}
	for _, h := range pending {
		v, ok := have[[3]string{h.alias, h.url, h.key}]
		if !ok {
			return "", fmt.Errorf("header %q of webhook %q in alias %q is %s but no secret is stored for it; write the real value", h.key, h.url, h.alias, NotifyRedacted)
		}
		h.val.Value, h.val.Style = v, 0
	}
	return encodeNotifyYAML(&doc)
}
