package api

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// EmailConfig holds Mailgun configuration.
type EmailConfig struct {
	Domain string // e.g., "w3ft.net" or "mg.w3ft.net"
	APIKey string // Mailgun API key
	From   string // e.g., "Weft <noreply@w3ft.net>"
}

// EmailConfigFromEnv loads email configuration from environment variables.
// Returns nil if MAILGUN_API_KEY is not set (dev mode — log links instead).
func EmailConfigFromEnv() *EmailConfig {
	apiKey := os.Getenv("MAILGUN_API_KEY")
	if apiKey == "" {
		return nil
	}
	domain := os.Getenv("MAILGUN_DOMAIN")
	if domain == "" {
		domain = "w3ft.net"
	}
	from := os.Getenv("MAILGUN_FROM")
	if from == "" {
		from = "Weft <noreply@" + domain + ">"
	}
	return &EmailConfig{
		Domain: domain,
		APIKey: apiKey,
		From:   from,
	}
}

// SendMagicLinkEmail sends the magic link login email via Mailgun HTTP API.
// If cfg is nil (dev mode), logs the link instead.
func SendMagicLinkEmail(cfg *EmailConfig, email, verifyURL string) error {
	if cfg == nil {
		log.Printf("magic link for %s: %s", email, verifyURL)
		return nil
	}

	endpoint := fmt.Sprintf("https://api.mailgun.net/v3/%s/messages", cfg.Domain)

	form := url.Values{
		"from":    {cfg.From},
		"to":      {email},
		"subject": {"Log in to Weft"},
		"html": {fmt.Sprintf(`
			<p>Click the link below to log in to Weft:</p>
			<p><a href="%s" style="display:inline-block;padding:12px 24px;background:#e67e22;color:white;text-decoration:none;border-radius:4px;font-weight:bold;">Log in to Weft</a></p>
			<p style="color:#888;font-size:12px;">This link expires in 15 minutes. If you didn't request this, ignore this email.</p>
		`, verifyURL)},
	}

	req, err := http.NewRequest("POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("api", cfg.APIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("mailgun request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("mailgun error: %s", resp.Status)
	}

	log.Printf("magic link email sent to %s", email)
	return nil
}
