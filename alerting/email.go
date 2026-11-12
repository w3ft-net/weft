package alerting

import (
	"context"
	"fmt"
	"net/smtp"
	"os"

	weftsdk "github.com/w3ft-net/weft-go"
	"gopkg.in/yaml.v3"

	"github.com/graybeardcorp/weft/store"
)

// SMTP is where an email alert is sent from.
type SMTP struct {
	Host     string
	Port     string
	User     string
	Password string
	From     string
}

// SMTPFromEnv reads EMAIL_HOST, EMAIL_PORT (default 587), EMAIL_HOST_USER,
// EMAIL_HOST_PASSWORD and EMAIL_FROM (default: the user). It returns nil when
// EMAIL_HOST or EMAIL_HOST_PASSWORD is unset: email alerts then fail to send
// with a message saying so.
func SMTPFromEnv() *SMTP {
	host, password := os.Getenv("EMAIL_HOST"), os.Getenv("EMAIL_HOST_PASSWORD")
	if host == "" || password == "" {
		return nil
	}
	c := &SMTP{Host: host, Port: os.Getenv("EMAIL_PORT"), User: os.Getenv("EMAIL_HOST_USER"), Password: password, From: os.Getenv("EMAIL_FROM")}
	if c.Port == "" {
		c.Port = "587"
	}
	if c.From == "" {
		c.From = c.User
	}
	return c
}

// EmailTarget sends a plain-text message over SMTP. A nil SMTP makes every
// send fail with a message saying so.
type EmailTarget struct {
	SMTP *SMTP
}

func (EmailTarget) Kind() string { return "email" }

func (e EmailTarget) Open(entry *yaml.Node) (Destination, error) {
	var t store.EmailTarget
	if err := entry.Decode(&t); err != nil {
		return Destination{}, err
	}
	return Destination{
		Key:  "email:" + t.Address,
		Name: t.Address,
		Send: func(ctx context.Context, a Alert) error { return e.send(ctx, a.Cron, t, a.Body) },
	}, nil
}

func (e EmailTarget) send(ctx context.Context, watchName string, target store.EmailTarget, body string) error {
	c := e.SMTP
	if c == nil {
		return fmt.Errorf("no SMTP configuration (set EMAIL_HOST and EMAIL_HOST_PASSWORD)")
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: weft alert: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		c.From, target.Address, watchName, Truncate(body, 4000))
	auth := smtp.PlainAuth("", c.User, c.Password, c.Host)
	if err := smtp.SendMail(c.Host+":"+c.Port, auth, c.User, []string{target.Address}, []byte(msg)); err != nil {
		return fmt.Errorf("email send: %w", err)
	}
	weftsdk.Infof(ctx, "notify: sent email alert for watch %s to %s", watchName, target.Address)
	return nil
}
