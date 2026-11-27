package magiclink

import (
	"context"
	"errors"
	"fmt"
	"net/smtp"

	"github.com/w3ft-net/weft/alerting"
)

// SMTPMailer sends the link over SMTP, with the settings an email alert uses
// (alerting.SMTPFromEnv: EMAIL_HOST, EMAIL_PORT, EMAIL_HOST_USER,
// EMAIL_HOST_PASSWORD, EMAIL_FROM).
type SMTPMailer struct{ SMTP *alerting.SMTP }

func (m SMTPMailer) Send(_ context.Context, to, subject, body string) error {
	c := m.SMTP
	if c == nil {
		return errors.New("no SMTP configuration (set EMAIL_HOST and EMAIL_HOST_PASSWORD)")
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s", c.From, to, subject, body)
	auth := smtp.PlainAuth("", c.User, c.Password, c.Host)
	return smtp.SendMail(c.Host+":"+c.Port, auth, c.From, []string{to}, []byte(msg))
}
