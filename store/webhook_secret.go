package store

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gorm.io/gorm"
)

// WebhookSecret is a named credential for webhook targets. notify.yaml refers
// to it as ${secrets.NAME} in a webhook URL or header value; the value itself
// is never written into the document and never read back out.
type WebhookSecret struct {
	gorm.Model
	OrgID OrgID  `gorm:"not null;index:idx_webhook_secrets_org_name,unique,where:deleted_at IS NULL"`
	Name  string `gorm:"not null;index:idx_webhook_secrets_org_name,unique,where:deleted_at IS NULL"`
	Value string `gorm:"not null"`
}

func (WebhookSecret) TableName() string { return "webhook_secrets" }

const secretNamePattern = `[A-Za-z0-9_-]+`

var (
	secretNameRe = regexp.MustCompile(`^` + secretNamePattern + `$`)
	secretRefRe  = regexp.MustCompile(`\$\{secrets\.(` + secretNamePattern + `)\}`)
)

// SaveWebhookSecret creates or replaces a named secret.
func (s *Store) SaveWebhookSecret(orgID OrgID, name, value string) error {
	if !secretNameRe.MatchString(name) {
		return fmt.Errorf("secret name %q: use letters, digits, '_' and '-'", name)
	}
	if value == "" {
		return fmt.Errorf("secret %q: value is empty", name)
	}
	var existing WebhookSecret
	err := s.scoped(orgID).Where("name = ?", name).First(&existing).Error
	if err == gorm.ErrRecordNotFound {
		return s.scoped(orgID).Create(&WebhookSecret{OrgID: orgID, Name: name, Value: value}).Error
	}
	if err != nil {
		return err
	}
	return s.scoped(orgID).Model(&existing).Update("value", value).Error
}

// DeleteWebhookSecret removes a secret outright (not a soft delete: the value
// must not linger), unless notify.yaml still refers to it.
func (s *Store) DeleteWebhookSecret(orgID OrgID, name string) error {
	if nc, err := s.GetNotifyConfig(orgID); err == nil {
		if aliases, err := ParseNotifyAliases(nc.AliasesYAML); err == nil {
			for alias, a := range aliases {
				for _, t := range a.Webhook {
					for _, ref := range t.secretRefs() {
						if ref == name {
							return fmt.Errorf("secret %q is still used by alias %q in /etc/notify.yaml; remove the reference first", name, alias)
						}
					}
				}
			}
		}
	}
	res := s.scoped(orgID).Unscoped().Where("name = ?", name).Delete(&WebhookSecret{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("no webhook secret named %q", name)
	}
	return nil
}

// ListWebhookSecretNames returns the org's secret names, sorted. Values are
// never returned.
func (s *Store) ListWebhookSecretNames(orgID OrgID) ([]string, error) {
	var names []string
	err := s.scoped(orgID).Model(&WebhookSecret{}).Order("name").Pluck("name", &names).Error
	return names, err
}

// secretRefs lists the secret names a target's URL and header values mention.
func (t WebhookTarget) secretRefs() []string {
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

// checkSecretRefs refuses a notify.yaml that refers to a secret the org hasn't
// defined, naming the alias and how to define it.
func (s *Store) checkSecretRefs(orgID OrgID, aliases AliasMap) error {
	have, err := s.ListWebhookSecretNames(orgID)
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
			for _, ref := range t.secretRefs() {
				if !known[ref] {
					return fmt.Errorf("notify.yaml: alias %q refers to ${secrets.%s}, which is not defined; define it with `weft integration webhook-secret set %s --value <value>`", alias, ref, ref)
				}
			}
		}
	}
	return nil
}

// ResolveWebhookTarget substitutes ${secrets.NAME} in the target's URL and
// header values. The returned replacer masks the substituted values, for
// scrubbing error text that would otherwise echo a secret back (an HTTP
// client's error carries the full URL).
func (s *Store) ResolveWebhookTarget(orgID OrgID, t WebhookTarget) (WebhookTarget, *strings.Replacer, error) {
	refs := t.secretRefs()
	if len(refs) == 0 {
		return t, strings.NewReplacer(), nil
	}
	vals := map[string]string{}
	for _, name := range refs {
		var sec WebhookSecret
		if err := s.scoped(orgID).Where("name = ?", name).First(&sec).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return t, nil, fmt.Errorf("webhook secret %q is not defined", name)
			}
			return t, nil, err
		}
		vals[name] = sec.Value
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
