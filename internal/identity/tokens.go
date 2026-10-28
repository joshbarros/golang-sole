package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// TokenConfig is what the token service needs to know.
type TokenConfig struct {
	Key           string // HMAC-SHA256 signing key (HS256), at least 32 bytes
	RSAPrivatePEM string // when set, tokens are signed with RS256 and the public key is published at /.well-known/jwks.json
	KeyID         string
	Issuer        string
	Audience      string
	AccessMinutes int
	RefreshDays   int
}

// AccessToken is a signed JWT and when it stops working.
type AccessToken struct {
	Value     string    `json:"value"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Claims is what an access token says.
type Claims struct {
	UserID  uuid.UUID
	Email   string
	Roles   []string
	Version int
}

// TokenService creates and validates tokens. It knows nothing about the database, so it is a plain unit-test target.
type TokenService struct {
	cfg     TokenConfig
	rsa     *rsa.PrivateKey
	purpose []byte
	now     func() time.Time
}

// VersionClaim carries the user's session version.
const VersionClaim = "ver"

// NewTokenService validates the configuration and loads the RSA key if there is one.
func NewTokenService(cfg TokenConfig) (*TokenService, error) {
	t := &TokenService{cfg: cfg, now: time.Now}
	if strings.TrimSpace(cfg.RSAPrivatePEM) != "" {
		block, _ := pem.Decode([]byte(cfg.RSAPrivatePEM))
		if block == nil {
			return nil, errors.New("JWT_RSA_PRIVATE_KEY_PEM is not PEM")
		}
		key, err := parseRSA(block)
		if err != nil {
			return nil, err
		}
		t.rsa = key
		sum := sha256.Sum256(x509.MarshalPKCS1PrivateKey(key))
		t.purpose = sum[:]
	} else {
		if len(cfg.Key) < 32 {
			return nil, errors.New("the signing key must be at least 32 bytes")
		}
		t.purpose = []byte(cfg.Key)
	}
	return t, nil
}

func parseRSA(block *pem.Block) (*rsa.PrivateKey, error) {
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the RSA key cannot be parsed: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the key is not an RSA key")
	}
	return rk, nil
}

// SetClock replaces the clock (tests).
func (t *TokenService) SetClock(now func() time.Time) { t.now = now }

// UsesRSA is true when tokens are signed with RS256.
func (t *TokenService) UsesRSA() bool { return t.rsa != nil }

// CreateAccessToken signs a short-lived JWT.
func (t *TokenService) CreateAccessToken(c Claims) (AccessToken, error) {
	now := t.now().UTC()
	expires := now.Add(time.Duration(t.cfg.AccessMinutes) * time.Minute)
	claims := jwt.MapClaims{
		"sub":        c.UserID.String(),
		"email":      c.Email,
		"role":       c.Roles,
		"jti":        uuid.NewString(),
		"iss":        t.cfg.Issuer,
		"aud":        t.cfg.Audience,
		"iat":        now.Unix(),
		"nbf":        now.Unix(),
		"exp":        expires.Unix(),
		VersionClaim: strconv.Itoa(c.Version),
	}
	var token *jwt.Token
	var signed string
	var err error
	if t.rsa != nil {
		token = jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = t.cfg.KeyID
		signed, err = token.SignedString(t.rsa)
	} else {
		token = jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		signed, err = token.SignedString([]byte(t.cfg.Key))
	}
	if err != nil {
		return AccessToken{}, err
	}
	return AccessToken{Value: signed, ExpiresAt: expires}, nil
}

// ErrBadToken is returned for any access token that must not be accepted.
var ErrBadToken = errors.New("invalid token")

// Parse validates signature, algorithm, issuer, audience and lifetime (30 s of clock skew) and returns the claims.
func (t *TokenService) Parse(raw string) (Claims, error) {
	method := jwt.SigningMethodHS256.Alg()
	var keyFunc jwt.Keyfunc = func(*jwt.Token) (any, error) { return []byte(t.cfg.Key), nil }
	if t.rsa != nil {
		method = jwt.SigningMethodRS256.Alg()
		keyFunc = func(*jwt.Token) (any, error) { return &t.rsa.PublicKey, nil }
	}
	parsed, err := jwt.Parse(raw, keyFunc,
		jwt.WithValidMethods([]string{method}),
		jwt.WithIssuer(t.cfg.Issuer),
		jwt.WithAudience(t.cfg.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
		jwt.WithTimeFunc(t.now),
	)
	if err != nil || !parsed.Valid {
		return Claims{}, ErrBadToken
	}
	m, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return Claims{}, ErrBadToken
	}
	sub, _ := m["sub"].(string)
	id, err := uuid.Parse(sub)
	if err != nil {
		return Claims{}, ErrBadToken
	}
	c := Claims{UserID: id}
	c.Email, _ = m["email"].(string)
	switch roles := m["role"].(type) {
	case []any:
		for _, r := range roles {
			if s, ok := r.(string); ok {
				c.Roles = append(c.Roles, s)
			}
		}
	case string:
		c.Roles = []string{roles}
	}
	ver, _ := m[VersionClaim].(string)
	if _, err := fmt.Sscanf(ver, "%d", &c.Version); err != nil {
		return Claims{}, ErrBadToken
	}
	return c, nil
}

// JWKS is the public key set, or nil when tokens are signed with a shared secret.
func (t *TokenService) JWKS() map[string]any {
	if t.rsa == nil {
		return nil
	}
	pub := t.rsa.PublicKey
	return map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": t.cfg.KeyID,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}}
}

// NewRefreshToken is 48 random bytes, URL-safe. The raw value goes to the client once; only its hash is stored.
func NewRefreshToken() string {
	b := make([]byte, 48)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken is the hex SHA-256 used to store refresh tokens.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ---- one-time email tokens (confirm an address, reset a password) ----

// Purposes of the one-time tokens.
const (
	PurposeConfirmEmail  = "confirm-email"
	PurposeResetPassword = "reset-password"
)

// PurposeToken is bound to the user and to the state they are in: the security stamp changes when the password does, and
// the email-confirmed flag flips on confirmation, so each token works once and dies when its purpose is served.
func (t *TokenService) PurposeToken(purpose string, u User, ttl time.Duration) string {
	exp := t.now().Add(ttl).Unix()
	payload := fmt.Sprintf("%s|%s|%d", purpose, u.ID, exp)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(t.purposeMAC(payload, u))
}

func (t *TokenService) purposeMAC(payload string, u User) []byte {
	mac := hmac.New(sha256.New, t.purpose)
	_, _ = mac.Write([]byte(payload + "|" + u.SecurityStamp + "|" + strconv.FormatBool(u.EmailConfirmed)))
	return mac.Sum(nil)
}

// VerifyPurposeToken is true only for an unexpired token issued for this purpose, this user and this state.
func (t *TokenService) VerifyPurposeToken(purpose, token string, u User) bool {
	payloadB64, macB64, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(macB64)
	if err != nil {
		return false
	}
	parts := strings.Split(string(payload), "|")
	if len(parts) != 3 || parts[0] != purpose || parts[1] != u.ID.String() {
		return false
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || t.now().Unix() > exp {
		return false
	}
	return subtle.ConstantTimeCompare(got, t.purposeMAC(string(payload), u)) == 1
}
