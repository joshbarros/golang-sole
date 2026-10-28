package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 authenticator apps use HMAC-SHA1
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh 160-bit shared key, base32 encoded the way authenticator apps expect.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return b32.EncodeToString(b)
}

// TOTPCode is RFC 6238 (SHA-1, 30 seconds, 6 digits).
func TOTPCode(secret string, at time.Time) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", err
	}
	counter := uint64(at.Unix() / 30)
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset])&0x7f)<<24 | uint32(sum[offset+1])<<16 | uint32(sum[offset+2])<<8 | uint32(sum[offset+3])
	return fmt.Sprintf("%06d", value%1_000_000), nil
}

// ValidTOTP accepts the code for the current 30-second step and the one before and after it (clock drift).
func ValidTOTP(secret, code string, now time.Time) bool {
	code = strings.ReplaceAll(strings.ReplaceAll(code, " ", ""), "-", "")
	if len(code) != 6 {
		return false
	}
	for step := -1; step <= 1; step++ {
		want, err := TOTPCode(secret, now.Add(time.Duration(step)*30*time.Second))
		if err != nil {
			return false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// OtpAuthURI is what a QR code carries.
func OtpAuthURI(app, email, secret string) string {
	issuer := url.PathEscape(app)
	return fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s&digits=6", issuer, url.PathEscape(email), secret, issuer)
}

// NewRecoveryCodes returns n one-time codes like "k3j9d-x82hf".
func NewRecoveryCodes(n int) []string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	codes := make([]string, n)
	for i := range codes {
		b := make([]byte, 10)
		_, _ = rand.Read(b)
		for j := range b {
			b[j] = alphabet[int(b[j])%len(alphabet)]
		}
		codes[i] = string(b[:5]) + "-" + string(b[5:])
	}
	return codes
}
