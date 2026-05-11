package email

import (
	"fmt"
	"net/smtp"
	"os"
)

// Config holds SMTP configuration for sending transactional emails.
type Config struct {
	Host     string // SMTP host (default: smtp.mailgun.org)
	Port     string // SMTP port (default: 587)
	User     string // SMTP username
	Password string // SMTP password
	From     string // From address
}

// ConfigFromEnv loads SMTP configuration from environment variables.
// Returns nil if EMAIL_HOST_PASSWORD is not set (dev mode — log links instead).
func ConfigFromEnv() *Config {
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
	return &Config{
		Host:     host,
		Port:     port,
		User:     user,
		Password: password,
		From:     from,
	}
}

// Send sends an email with the given subject and HTML body.
func Send(cfg *Config, to, subject, htmlBody string) error {
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n%s",
		cfg.From, to, subject, htmlBody)

	auth := smtp.PlainAuth("", cfg.User, cfg.Password, cfg.Host)
	addr := cfg.Host + ":" + cfg.Port

	return smtp.SendMail(addr, auth, cfg.User, []string{to}, []byte(msg))
}

// SendPlain sends a plain-text email.
func SendPlain(cfg *Config, to, subject, body string) error {
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		cfg.From, to, subject, body)

	auth := smtp.PlainAuth("", cfg.User, cfg.Password, cfg.Host)
	addr := cfg.Host + ":" + cfg.Port

	return smtp.SendMail(addr, auth, cfg.User, []string{to}, []byte(msg))
}
