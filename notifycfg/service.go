package notifycfg

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/w3ft-net/weft/backends/notify"
	"github.com/w3ft-net/weft/shape"
)

const secretNamePattern = `[A-Za-z0-9_-]+`

var (
	secretNameRe = regexp.MustCompile(`^` + secretNamePattern + `$`)
	secretRefRe  = regexp.MustCompile(`\$\{secrets\.(` + secretNamePattern + `)\}`)
)

// SecretRefs lists the secret names the target's URL and header values mention.
func (t WebhookTarget) SecretRefs() []string {
	var refs []string
	add := func(v string) {
		for _, m := range secretRefRe.FindAllStringSubmatch(v, -1) {
			refs = append(refs, m[1])
		}
	}
	add(t.URL)
	for _, v := range t.Headers {
		add(v)
	}
	return refs
}

// Service is what a build does with the notification configuration beyond
// storing it: it judges a document before it is saved, and keeps the secrets its
// webhook targets refer to consistent with it.
type Service struct {
	Aliases notify.Store
	Secrets notify.Secrets
}

// Save validates the document and writes it: it must parse, keep the system
// alias, and refer only to secrets the owner has defined.
func (s Service) Save(owner shape.Added, user, text string) error {
	if err := Validate(text); err != nil {
		return err
	}
	aliases, _ := Parse(text) // parsed once already by the validation above
	if err := s.checkSecretRefs(owner, aliases); err != nil {
		return err
	}
	return s.Aliases.SaveAliases(owner, user, text)
}

// Current is the owner's alias map, empty if none was ever saved.
func (s Service) Current(owner shape.Added) (AliasMap, error) {
	doc, err := s.Aliases.Aliases(owner)
	if err != nil {
		return nil, err
	}
	return Parse(doc.Text)
}

// checkSecretRefs refuses a document that refers to a secret the owner hasn't
// defined, naming the alias and how to define it.
func (s Service) checkSecretRefs(owner shape.Added, aliases AliasMap) error {
	have, err := s.Secrets.Names(owner)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, n := range have {
		known[n] = true
	}
	names := aliases.AliasNames()
	sort.Strings(names)
	for _, alias := range names {
		for _, t := range aliases[alias].Webhook {
			for _, ref := range t.SecretRefs() {
				if !known[ref] {
					return fmt.Errorf("notify.yaml: alias %q refers to ${secrets.%s}, which is not defined; define it with `weft secret set %s --value <value>`", alias, ref, ref)
				}
			}
		}
		var tokens []string
		for _, t := range aliases[alias].Slack {
			tokens = append(tokens, t.Token)
		}
		for _, t := range aliases[alias].PagerDuty {
			tokens = append(tokens, t.Token)
		}
		for _, tok := range tokens {
			if name := TokenSecret(tok, ""); name != "" && !known[name] {
				return fmt.Errorf("notify.yaml: alias %q refers to ${secrets.%s}, which is not defined; define it with `weft secret set %s --value <value>`", alias, name, name)
			}
		}
	}
	return nil
}

// SetSecret creates or replaces a named secret.
func (s Service) SetSecret(owner shape.Added, name, value string) error {
	if !secretNameRe.MatchString(name) {
		return fmt.Errorf("secret name %q: use letters, digits, '_' and '-'", name)
	}
	if value == "" {
		return fmt.Errorf("secret %q: value is empty", name)
	}
	return s.Secrets.Set(owner, name, value)
}

// DeleteSecret removes a secret outright, unless the owner's notify.yaml still
// refers to it.
func (s Service) DeleteSecret(owner shape.Added, name string) error {
	if aliases, err := s.Current(owner); err == nil {
		for alias, a := range aliases {
			for _, t := range a.Webhook {
				for _, ref := range t.SecretRefs() {
					if ref == name {
						return fmt.Errorf("secret %q is still used by alias %q in /etc/notify.yaml; remove the reference first", name, alias)
					}
				}
			}
		}
	}
	if err := s.Secrets.Delete(owner, name); errors.Is(err, notify.ErrNotFound) {
		return fmt.Errorf("no webhook secret named %q", name)
	} else {
		return err
	}
}

// Secret is the value of a named secret.
func (s Service) Secret(owner shape.Added, name string) (string, error) {
	v, err := s.Secrets.Value(owner, name)
	if errors.Is(err, notify.ErrNotFound) {
		return "", fmt.Errorf("secret %q is not defined; define it with `weft secret set %s --value <value>`", name, name)
	}
	return v, err
}

// ResolveWebhook substitutes ${secrets.NAME} in the target's URL and header
// values. The returned replacer masks the substituted values, for scrubbing error
// text that would otherwise echo a secret back (an HTTP client's error carries
// the full URL).
func (s Service) ResolveWebhook(owner shape.Added, t WebhookTarget) (WebhookTarget, *strings.Replacer, error) {
	refs := t.SecretRefs()
	if len(refs) == 0 {
		return t, strings.NewReplacer(), nil
	}
	vals := map[string]string{}
	for _, name := range refs {
		v, err := s.Secrets.Value(owner, name)
		if errors.Is(err, notify.ErrNotFound) {
			return t, nil, fmt.Errorf("webhook secret %q is not defined", name)
		}
		if err != nil {
			return t, nil, err
		}
		vals[name] = v
	}
	var mask []string
	for name, v := range vals {
		mask = append(mask, v, "${secrets."+name+"}")
	}
	expand := func(v string) string {
		return secretRefRe.ReplaceAllStringFunc(v, func(m string) string {
			return vals[secretRefRe.FindStringSubmatch(m)[1]]
		})
	}
	out := WebhookTarget{URL: expand(t.URL)}
	if len(t.Headers) > 0 {
		out.Headers = make(map[string]string, len(t.Headers))
		for k, v := range t.Headers {
			out.Headers[k] = expand(v)
		}
	}
	return out, strings.NewReplacer(mask...), nil
}
