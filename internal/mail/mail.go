// Package mail sends transactional email over SMTP with STARTTLS — plain
// stdlib, compatible with Brevo, Mailgun, SES-SMTP, Postmark and friends.
package mail

import (
	"fmt"
	"log"
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
