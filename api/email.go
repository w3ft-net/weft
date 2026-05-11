package api

import (
	"fmt"
	"log"

	"github.com/kevlarcon/weft/internal/email"
)

// EmailConfig is an alias for the shared email.Config, re-exported for
// backward compatibility within the api package.
type EmailConfig = email.Config

// EmailConfigFromEnv loads SMTP configuration from environment variables.
func EmailConfigFromEnv() *EmailConfig {
	return email.ConfigFromEnv()
}

// SendMagicLinkEmail sends the magic link login email via SMTP.
// If cfg is nil (dev mode), logs the link instead.
func SendMagicLinkEmail(cfg *EmailConfig, addr, verifyURL string) error {
	if cfg == nil {
		log.Printf("magic link for %s: %s", addr, verifyURL)
		return nil
	}

	body := fmt.Sprintf(`<p>Click the link below to log in to Weft:</p>
<p><a href="%s" style="display:inline-block;padding:12px 24px;background:#e67e22;color:white;text-decoration:none;border-radius:4px;font-weight:bold;">Log in to Weft</a></p>
<p style="color:#888;font-size:12px;">This link expires in 15 minutes. If you didn't request this, ignore this email.</p>`, verifyURL)

	err := email.Send(cfg, addr, "Log in to Weft", body)
	if err != nil {
		return err
	}
	log.Printf("magic link email sent to %s", addr)
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

	err := email.Send(cfg, toEmail, fmt.Sprintf("You've been invited to %s on Weft", customerName), body)
	if err != nil {
		return err
	}
	log.Printf("invite email sent to %s (team: %s)", toEmail, customerName)
	return nil
}
