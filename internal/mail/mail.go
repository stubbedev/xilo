// Package mail sends transactional email over SMTP with STARTTLS — plain
// stdlib, compatible with Brevo, Mailgun, SES-SMTP, Postmark and friends.
package mail

import (
	"errors"
	"fmt"
	"log"
	netmail "net/mail"
	"net/smtp"
	"strings"
	"time"
)

// Config is the SMTP relay. Zero Host = mail disabled (every send becomes a
// silent no-op so callers never need to branch).
type Config struct {
	// SMTP relay host, e.g. "smtp-relay.brevo.com". Empty disables mail.
	Host string `yaml:"host" json:"host"`
	// Relay port; 587 (STARTTLS) is the standard submission port.
	Port int `yaml:"port" json:"port"`
	// SMTP username (Brevo: your account login).
	Username string `yaml:"username" json:"username"`
	// SMTP password / API key. Overridable with XILO_SMTP_PASSWORD.
	Password string `yaml:"password" json:"password"`
	// From address, e.g. "Xilo <cache@example.com>". Must be a sender your
	// relay allows.
	From string `yaml:"from" json:"from"`
}

// Enabled reports whether sending is configured.
func (c Config) Enabled() bool { return c.Host != "" && c.From != "" }

// Send delivers one plain-text message synchronously. Returns nil without
// doing anything when mail is disabled or the recipient is empty.
func Send(c Config, to, subject, body string) error {
	if !c.Enabled() || to == "" {
		return nil
	}
	// Reject header-injection at the boundary: a control character in any
	// header field would let a caller inject extra SMTP headers (Bcc, spoofed
	// From, …) or corrupt the envelope commands, so only printable runes pass.
	for _, h := range []string{to, subject, c.From} {
		if strings.IndexFunc(h, func(r rune) bool { return r < ' ' || r == 0x7f }) >= 0 {
			return errors.New("mail: header field contains a control character")
		}
	}
	// The recipient is also the envelope destination: an address that does
	// not parse is a relay-abuse attempt, not a deliverable mailbox.
	if _, err := netmail.ParseAddress(to); err != nil {
		return fmt.Errorf("mail: invalid recipient %q: %w", to, err)
	}
	port := c.Port
	if port == 0 {
		port = 587
	}
	addr := fmt.Sprintf("%s:%d", c.Host, port)
	var auth smtp.Auth
	if c.Username != "" {
		auth = smtp.PlainAuth("", c.Username, c.Password, c.Host)
	}
	msg := strings.Join([]string{
		"From: " + c.From,
		"To: " + to,
		"Subject: " + subject,
		"Date: " + time.Now().Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		body,
		"",
	}, "\r\n")
	// smtp.SendMail negotiates STARTTLS when the relay advertises it and
	// refuses to AUTH on plaintext connections — the right default for :587.
	return smtp.SendMail(addr, auth, fromAddr(c.From), []string{to}, []byte(msg))
}

// Go sends asynchronously, logging failures — transactional mail must never
// block or fail a request.
func Go(c Config, to, subject, body string) {
	if !c.Enabled() || to == "" {
		return
	}
	go func() {
		if err := Send(c, to, subject, body); err != nil {
			log.Printf("mail: send to %s failed: %v", to, err)
		}
	}()
}

// fromAddr extracts the bare address from "Name <addr>" for the envelope.
func fromAddr(from string) string {
	if i := strings.LastIndexByte(from, '<'); i >= 0 {
		if j := strings.IndexByte(from[i:], '>'); j > 0 {
			return from[i+1 : i+j]
		}
	}
	return from
}
