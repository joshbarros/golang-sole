package identity

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const testKey = "unit-test-signing-key-0123456789abcdef"

func newTokens(t *testing.T) *TokenService {
	t.Helper()
	svc, err := NewTokenService(TokenConfig{Key: testKey, KeyID: "k1", Issuer: "sole-api", Audience: "sole-web", AccessMinutes: 15, RefreshDays: 14})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func claims() Claims {
	return Claims{UserID: uuid.New(), Email: "ana@example.com", Roles: []string{"Customer"}, Version: 3}
}

func TestAccessTokenRoundTrip(t *testing.T) {
	svc := newTokens(t)
	want := claims()
	tok, err := svc.CreateAccessToken(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Parse(tok.Value)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != want.UserID || got.Email != want.Email || got.Version != 3 || len(got.Roles) != 1 || got.Roles[0] != "Customer" {
		t.Fatalf("round trip lost something: %+v", got)
	}
	if d := time.Until(tok.ExpiresAt); d < 14*time.Minute || d > 15*time.Minute {
		t.Fatalf("expiry should be about 15 minutes away, got %v", d)
	}
}

func TestParseRefusesWhatItMust(t *testing.T) {
	svc := newTokens(t)
	good, _ := svc.CreateAccessToken(claims())
	parts := strings.Split(good.Value, ".")
	cases := map[string]string{
		"garbage":      "not-a-jwt",
		"empty":        "",
		"tampered":     parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-2] + "xx",
		"no signature": parts[0] + "." + parts[1] + ".",
	}
	for name, tok := range cases {
		if _, err := svc.Parse(tok); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}

	other, _ := NewTokenService(TokenConfig{Key: "a-different-signing-key-0123456789abcdef", Issuer: "sole-api", Audience: "sole-web", AccessMinutes: 15})
	foreign, _ := other.CreateAccessToken(claims())
	if _, err := svc.Parse(foreign.Value); err == nil {
		t.Error("a token signed with another key must be refused")
	}
	wrongIssuer, _ := NewTokenService(TokenConfig{Key: testKey, Issuer: "someone-else", Audience: "sole-web", AccessMinutes: 15})
	tok, _ := wrongIssuer.CreateAccessToken(claims())
	if _, err := svc.Parse(tok.Value); err == nil {
		t.Error("a token from another issuer must be refused")
	}
	wrongAudience, _ := NewTokenService(TokenConfig{Key: testKey, Issuer: "sole-api", Audience: "other-app", AccessMinutes: 15})
	tok, _ = wrongAudience.CreateAccessToken(claims())
	if _, err := svc.Parse(tok.Value); err == nil {
		t.Error("a token for another audience must be refused")
	}
}

func TestExpiredTokenIsRefusedBeyondTheSkew(t *testing.T) {
	svc := newTokens(t)
	start := time.Now()
	svc.SetClock(func() time.Time { return start.Add(-16 * time.Minute) }) // minted 16 minutes ago: expired 1 minute ago
	old, _ := svc.CreateAccessToken(claims())
	svc.SetClock(time.Now)
	if _, err := svc.Parse(old.Value); err == nil {
		t.Fatal("an expired token must be refused")
	}
	svc.SetClock(func() time.Time { return start.Add(-15*time.Minute - 10*time.Second) }) // expired 10 s ago: inside the 30 s skew
	recent, _ := svc.CreateAccessToken(claims())
	svc.SetClock(time.Now)
	if _, err := svc.Parse(recent.Value); err != nil {
		t.Fatalf("a token expired 10 s ago is inside the clock skew: %v", err)
	}
}

func TestOtherAlgorithmsAreRefused(t *testing.T) {
	svc := newTokens(t)
	c := jwt.MapClaims{"sub": uuid.NewString(), "iss": "sole-api", "aud": "sole-web", "exp": time.Now().Add(time.Hour).Unix(), "ver": "0"}
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, c).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := svc.Parse(none); err == nil {
		t.Fatal("alg=none must be refused")
	}
	hs512, _ := jwt.NewWithClaims(jwt.SigningMethodHS512, c).SignedString([]byte(testKey))
	if _, err := svc.Parse(hs512); err == nil {
		t.Fatal("HS512 must be refused when the service signs with HS256")
	}
}

func TestShortKeyIsRefusedAtStartup(t *testing.T) {
	if _, err := NewTokenService(TokenConfig{Key: "too-short", Issuer: "i", Audience: "a", AccessMinutes: 15}); err == nil {
		t.Fatal("a short signing key must be refused")
	}
}

func rsaPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func TestRS256PublishesItsPublicKeyAndRefusesHS256(t *testing.T) {
	svc, err := NewTokenService(TokenConfig{RSAPrivatePEM: rsaPEM(t), KeyID: "rsa-1", Issuer: "sole-api", Audience: "sole-web", AccessMinutes: 15})
	if err != nil {
		t.Fatal(err)
	}
	if !svc.UsesRSA() || svc.JWKS() == nil {
		t.Fatal("RSA mode must publish a key set")
	}
	tok, _ := svc.CreateAccessToken(claims())
	parsed, _, _ := jwt.NewParser().ParseUnverified(tok.Value, jwt.MapClaims{})
	if parsed.Header["alg"] != "RS256" || parsed.Header["kid"] != "rsa-1" {
		t.Fatalf("unexpected header %v", parsed.Header)
	}
	if _, err := svc.Parse(tok.Value); err != nil {
		t.Fatalf("RS256 round trip: %v", err)
	}
	hs := newTokens(t)
	forged, _ := hs.CreateAccessToken(claims())
	if _, err := svc.Parse(forged.Value); err == nil {
		t.Fatal("an HS256 token must not pass in RS256 mode (algorithm confusion)")
	}
	if newTokens(t).JWKS() != nil {
		t.Fatal("HS256 mode has no public key to publish")
	}
}

func TestRefreshTokensAreRandomAndOnlyTheirHashIsStored(t *testing.T) {
	a, b := NewRefreshToken(), NewRefreshToken()
	if a == b || len(a) < 60 {
		t.Fatalf("refresh tokens must be long and unique: %q %q", a, b)
	}
	if HashToken(a) == a || len(HashToken(a)) != 64 || HashToken(a) != HashToken(a) {
		t.Fatal("the stored value must be a stable 64-hex SHA-256")
	}
}

func TestPurposeTokensAreSingleUseAndBoundToTheirUser(t *testing.T) {
	svc := newTokens(t)
	u := User{ID: uuid.New(), SecurityStamp: "stamp-1"}
	tok := svc.PurposeToken(PurposeResetPassword, u, time.Hour)
	if !svc.VerifyPurposeToken(PurposeResetPassword, tok, u) {
		t.Fatal("a fresh token must verify")
	}
	if svc.VerifyPurposeToken(PurposeConfirmEmail, tok, u) {
		t.Error("a token must not work for another purpose")
	}
	if svc.VerifyPurposeToken(PurposeResetPassword, tok, User{ID: uuid.New(), SecurityStamp: "stamp-1"}) {
		t.Error("a token must not work for another user")
	}
	rotated := u
	rotated.SecurityStamp = "stamp-2" // the password changed
	if svc.VerifyPurposeToken(PurposeResetPassword, tok, rotated) {
		t.Error("a reset link must die when the password changes")
	}
	confirm := svc.PurposeToken(PurposeConfirmEmail, u, time.Hour)
	confirmed := u
	confirmed.EmailConfirmed = true
	if svc.VerifyPurposeToken(PurposeConfirmEmail, confirm, confirmed) {
		t.Error("a confirmation link must die once the address is confirmed")
	}
	if svc.VerifyPurposeToken(PurposeResetPassword, tok+"x", u) || svc.VerifyPurposeToken(PurposeResetPassword, "", u) || svc.VerifyPurposeToken(PurposeResetPassword, "a.b", u) {
		t.Error("garbage must not verify")
	}
	svc.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	if svc.VerifyPurposeToken(PurposeResetPassword, tok, u) {
		t.Error("an expired token must not verify")
	}
}
