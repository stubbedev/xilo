package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
)

// newTOTPSecret returns 20 random bytes (160-bit) for HMAC-SHA1 TOTP.
func newTOTPSecret() ([]byte, error) {
	b := make([]byte, 20)
	_, err := rand.Read(b)
	return b, err
}

// totpCode computes the RFC 6238 6-digit code for a 30s step (stdlib only).
func totpCode(secret []byte, t time.Time) string {
	counter := uint64(t.Unix()) / 30
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, secret)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%06d", code)
}

// totpVerify accepts a code within ±1 step (clock skew).
func totpVerify(secret []byte, code string, t time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return false
	}
	for _, skew := range []int{-1, 0, 1} {
		if hmac.Equal([]byte(totpCode(secret, t.Add(time.Duration(skew)*30*time.Second))), []byte(code)) {
			return true
		}
	}
	return false
}

// totpURI is the otpauth:// URI for authenticator enrollment.
func totpURI(secret []byte, issuer, account string) string {
	b32 := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
	return fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s",
		url.PathEscape(issuer), url.PathEscape(account), b32, url.QueryEscape(issuer))
}

// secretB32 formats the secret for manual authenticator entry.
func secretB32(secret []byte) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
}

// totpQRDataURI renders the enrollment URI as a PNG data: URI for an <img>.
func totpQRDataURI(uri string) (string, error) {
	png, err := qrcode.Encode(uri, qrcode.Medium, 256)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}
