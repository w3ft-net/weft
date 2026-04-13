package api

import (
	"fmt"
	"log"
	"net/smtp"
	"os"
)

// EmailConfig holds SMTP configuration for sending transactional emails.
type EmailConfig struct {
	Host     string // SMTP host (default: smtp.mailgun.org)
	Port     string // SMTP port (default: 587)
	User     string // SMTP username
	Password string // SMTP password
	From     string // From address
}

// EmailConfigFromEnv loads SMTP configuration from environment variables.
// Returns nil if EMAIL_HOST_PASSWORD is not set (dev mode — log links instead).
func EmailConfigFromEnv() *EmailConfig {
	password := os.Getenv("EMAIL_HOST_PASSWORD")
	if password == "" {
		return nil
	}
	host := os.Getenv("EMAIL_HOST")
	if host == "" {
		host = "smtp.mailgun.org"
	}
	port := os.Getenv("EMAIL_PORT")
	if port == "" {
		port = "587"
	}
	user := os.Getenv("EMAIL_HOST_USER")
	if user == "" {
		user = "no-reply@mg.w3ft.net"
	}
	from := os.Getenv("EMAIL_FROM")
	if from == "" {
		from = "Weft <noreply@mg.w3ft.net>"
	}
	return &EmailConfig{
		Host:     host,
		Port:     port,
		User:     user,
		Password: password,
		From:     from,
	}
}

// SendMagicLinkEmail sends the magic link login email via SMTP.
// If cfg is nil (dev mode), logs the link instead.
func SendMagicLinkEmail(cfg *EmailConfig, email, verifyURL string) error {
	if cfg == nil {
		log.Printf("magic link for %s: %s", email, verifyURL)
		return nil
	}

	body := fmt.Sprintf(`<p>Click the link below to log in to Weft:</p>
<p><a href="%s" style="display:inline-block;padding:12px 24px;background:#e67e22;color:white;text-decoration:none;border-radius:4px;font-weight:bold;">Log in to Weft</a></p>
<p style="color:#888;font-size:12px;">This link expires in 15 minutes. If you didn't request this, ignore this email.</p>`, verifyURL)

	err := sendEmail(cfg, email, "Log in to Weft", body)
	if err != nil {
		return err
	}
	log.Printf("magic link email sent to %s", email)
	return nil
}

// SendInviteEmail sends a team invite email via SMTP.
// If cfg is nil (dev mode), logs the link instead.
func SendInviteEmail(cfg *EmailConfig, toEmail, inviterEmail, customerName, verifyURL string) error {
	if cfg == nil {
		log.Printf("invite for %s to join %s (from %s): %s", toEmail, customerName, inviterEmail, verifyURL)
		return nil
	}

	body := fmt.Sprintf(`<p><strong>%s</strong> invited you to join <strong>%s</strong> on Weft.</p>
<p><a href="%s" style="display:inline-block;padding:12px 24px;background:#e67e22;color:white;text-decoration:none;border-radius:4px;font-weight:bold;">Accept Invite</a></p>
<p style="color:#888;font-size:12px;">This link expires in 15 minutes.</p>`, inviterEmail, customerName, verifyURL)

	err := sendEmail(cfg, toEmail, fmt.Sprintf("You've been invited to %s on Weft", customerName), body)
	if err != nil {
		return err
	}
	log.Printf("invite email sent to %s (team: %s)", toEmail, customerName)
	return nil
}

func sendEmail(cfg *EmailConfig, to, subject, htmlBody string) error {
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n%s",
		cfg.From, to, subject, htmlBody)

	auth := smtp.PlainAuth("", cfg.User, cfg.Password, cfg.Host)
	addr := cfg.Host + ":" + cfg.Port

	return smtp.SendMail(addr, auth, cfg.From, []string{to}, []byte(msg))
}
