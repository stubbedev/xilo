package config

import "testing"

func TestDatabasePostgres(t *testing.T) {
	for url, want := range map[string]bool{
		"postgres://u:p@h/db":   true,
		"postgresql://u:p@h/db": true,
		"":                      false,
		"/data/xilo.db":         false,
		"file:xilo.db":          false,
		"mysql://u:p@h/db":      false,
	} {
		if got := (Database{URL: url}).Postgres(); got != want {
			t.Errorf("Postgres(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestSMTPMail(t *testing.T) {
	s := SMTP{Host: "smtp.example.com", Port: 587, From: "x@y.com", Username: "u", Password: "p"}
	m := s.Mail()
	if m.Host != s.Host || m.Port != s.Port || m.From != s.From || m.Username != s.Username || m.Password != s.Password {
		t.Fatalf("Mail() dropped a field: %+v from %+v", m, s)
	}
}
