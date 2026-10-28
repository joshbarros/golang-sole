package identity

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1, secret "12345678901234567890" (the 8-digit codes cut to their last 6 digits).
func TestTOTPMatchesRFC6238(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037"} {
		got, err := TOTPCode(secret, time.Unix(unix, 0))
		if err != nil || got != want {
			t.Errorf("at %d got %q (%v), want %q", unix, got, err, want)
		}
	}
}

func TestValidTOTPAllowsOneStepOfDrift(t *testing.T) {
	secret := NewTOTPSecret()
	now := time.Unix(1_800_000_000, 0)
	for step, want := range map[int]bool{-1: true, 0: true, 1: true, -2: false, 2: false} {
		code, _ := TOTPCode(secret, now.Add(time.Duration(step)*30*time.Second))
		if got := ValidTOTP(secret, code, now); got != want {
			t.Errorf("step %d: got %v, want %v", step, got, want)
		}
	}
	if ValidTOTP(secret, "12345", now) || ValidTOTP(secret, "abcdef", now) || ValidTOTP(secret, "", now) {
		t.Error("a malformed code must not validate")
	}
}

func TestSecretsAndRecoveryCodes(t *testing.T) {
	if a, b := NewTOTPSecret(), NewTOTPSecret(); a == b || len(a) != 32 {
		t.Fatalf("secrets must be 160 random bits: %q %q", a, b)
	}
	codes := NewRecoveryCodes(8)
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != 11 || c[5] != '-' || seen[c] {
			t.Fatalf("bad or repeated recovery code %q", c)
		}
		seen[c] = true
	}
	if uri := OtpAuthURI("Sole", "a@b.co", "SECRET"); !strings.HasPrefix(uri, "otpauth://totp/Sole:a@b.co?secret=SECRET&issuer=Sole") {
		t.Fatalf("unexpected uri %s", uri)
	}
}
