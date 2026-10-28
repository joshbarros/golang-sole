package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-sole/internal/platform/httpx"
)

// Options are the service's settings.
type Options struct {
	RequireConfirmedEmail bool
	PublicURL             string
	AppName               string
	RefreshDays           int
}

// Service is the sign-in logic: accounts, sessions, two-factor, email flows.
type Service struct {
	Store  *Store
	Tokens *TokenService
	Mailer Mailer
	Opts   Options
	Now    func() time.Time
}

// Session is what a successful sign-in returns.
type Session struct {
	Access           AccessToken
	RefreshToken     string
	RefreshExpiresAt time.Time
	User             UserInfo
}

// UserInfo is the public face of a user.
type UserInfo struct {
	ID    uuid.UUID `json:"id"`
	Email string    `json:"email"`
	Roles []string  `json:"roles"`
}

// MfaRequired is the message a sign-in gives when the account needs a code.
const MfaRequired = "A verification code is required."

const lockedMessage = "Too many failed attempts. Try again in 15 minutes."

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func isEmail(v string) bool {
	if strings.TrimSpace(v) == "" || len(v) > 254 {
		return false
	}
	a, err := mail.ParseAddress(v)
	return err == nil && a.Address == v
}

func requireReasonableLength(password string) error {
	if len(password) > MaxPasswordLength {
		return httpx.Fail(http.StatusBadRequest, "A password is at most %d characters.", MaxPasswordLength)
	}
	return nil
}

// Register creates a customer and mails a confirmation link. It returns the session, or nil when the address must be confirmed first.
func (s *Service) Register(ctx context.Context, email, password string) (*Session, error) {
	if !isEmail(email) {
		return nil, httpx.Fail(http.StatusBadRequest, "A valid email address is required.")
	}
	if password == "" {
		return nil, httpx.Fail(http.StatusBadRequest, "A password is required.")
	}
	if err := requireReasonableLength(password); err != nil {
		return nil, err
	}
	if problems := PasswordPolicy(password); len(problems) > 0 {
		return nil, httpx.Fail(http.StatusBadRequest, "%s", strings.Join(problems, " "))
	}
	id, _ := uuid.NewV7()
	u := User{ID: id, Email: email, PasswordHash: HashPassword(password), SecurityStamp: uuid.NewString()}
	if err := s.Store.CreateUser(ctx, u, httpx.RoleCustomer); err != nil {
		if errors.Is(err, ErrDuplicateEmail) {
			return nil, httpx.Fail(http.StatusConflict, "Email '%s' is already taken.", email)
		}
		return nil, err
	}
	u.Roles = []string{httpx.RoleCustomer}
	if err := s.sendConfirmation(ctx, u); err != nil {
		return nil, err
	}
	if s.Opts.RequireConfirmedEmail {
		return nil, nil
	}
	return s.issue(ctx, u, uuid.New(), "")
}

// Login signs in with a password (and a code when two-factor is on).
func (s *Service) Login(ctx context.Context, email, password, code string) (*Session, error) {
	invalid := httpx.Fail(http.StatusUnauthorized, "Invalid email or password.")
	if strings.TrimSpace(email) == "" || password == "" || len(password) > MaxPasswordLength {
		return nil, invalid
	}
	u, err := s.Store.ByEmail(ctx, email)
	if errors.Is(err, ErrNotFound) {
		// Spend the same time a real check would, so the answer does not reveal which addresses exist.
		VerifyPassword(password, dummyHash)
		return nil, invalid
	}
	if err != nil {
		return nil, err
	}
	if u.IsLockedOut(s.now()) {
		return nil, httpx.Fail(http.StatusTooManyRequests, lockedMessage)
	}
	if !VerifyPassword(password, u.PasswordHash) {
		return nil, s.fail(ctx, u, invalid)
	}
	if s.Opts.RequireConfirmedEmail && !u.EmailConfirmed {
		return nil, httpx.Fail(http.StatusForbidden, "Confirm your email address first. Check your inbox.")
	}
	if u.TwoFactorEnabled {
		if strings.TrimSpace(code) == "" {
			return nil, httpx.Fail(http.StatusUnauthorized, MfaRequired)
		}
		ok, err := s.verifyCode(ctx, u, code)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, s.fail(ctx, u, httpx.Fail(http.StatusUnauthorized, "Invalid verification code."))
		}
	}
	if err := s.Store.ResetFailures(ctx, u.ID); err != nil {
		return nil, err
	}
	return s.issue(ctx, u, uuid.New(), "")
}

var dummyHash = HashPassword("not-a-real-password-0")

func (s *Service) fail(ctx context.Context, u User, invalid *httpx.DomainError) error {
	locked, err := s.Store.RegisterFailure(ctx, u.ID)
	if err != nil {
		return err
	}
	if locked {
		return httpx.Fail(http.StatusTooManyRequests, lockedMessage)
	}
	return invalid
}

// verifyCode accepts an authenticator code or one of the one-time recovery codes.
func (s *Service) verifyCode(ctx context.Context, u User, code string) (bool, error) {
	if ValidTOTP(u.TOTPSecret, code, s.now()) {
		return true, nil
	}
	return s.Store.RedeemRecoveryCode(ctx, u.ID, code)
}

// ConfirmEmail confirms an address with the emailed token.
func (s *Service) ConfirmEmail(ctx context.Context, userID uuid.UUID, token string) error {
	invalid := httpx.Fail(http.StatusBadRequest, "This confirmation link is invalid or has expired.")
	u, err := s.Store.ByID(ctx, userID)
	if err != nil || token == "" || !s.Tokens.VerifyPurposeToken(PurposeConfirmEmail, token, u) {
		return invalid
	}
	return s.Store.ConfirmEmail(ctx, u.ID)
}

// ResendConfirmation always looks the same from outside, so it cannot be used to find out who has an account.
func (s *Service) ResendConfirmation(ctx context.Context, email string) error {
	if strings.TrimSpace(email) == "" {
		return nil
	}
	u, err := s.Store.ByEmail(ctx, email)
	if err != nil || u.EmailConfirmed {
		return nil
	}
	return s.sendConfirmation(ctx, u)
}

// ForgotPassword mails a reset link when the account exists, and says nothing either way.
func (s *Service) ForgotPassword(ctx context.Context, email string) error {
	if strings.TrimSpace(email) == "" {
		return nil
	}
	u, err := s.Store.ByEmail(ctx, email)
	if err != nil {
		return nil
	}
	token := s.Tokens.PurposeToken(PurposeResetPassword, u, time.Hour)
	link := fmt.Sprintf("%s/?reset=%s&token=%s", strings.TrimRight(s.Opts.PublicURL, "/"), u.ID, token)
	return s.Mailer.Send(ctx, MailMessage{
		To: u.Email, Subject: fmt.Sprintf("Reset your %s password", s.Opts.AppName), Link: link,
		Body: fmt.Sprintf("Open this link to choose a new password. It works once and expires soon.\n\n%s\n\nIf you did not ask for this, ignore this email.", link),
	})
}

// ResetPassword sets a new password with the emailed token and ends every session.
func (s *Service) ResetPassword(ctx context.Context, userID uuid.UUID, token, newPassword string) error {
	invalid := httpx.Fail(http.StatusBadRequest, "This reset link is invalid or has expired.")
	u, err := s.Store.ByID(ctx, userID)
	if err != nil || token == "" || newPassword == "" {
		return invalid
	}
	if err := requireReasonableLength(newPassword); err != nil {
		return err
	}
	if !s.Tokens.VerifyPurposeToken(PurposeResetPassword, token, u) {
		return invalid
	}
	if problems := PasswordPolicy(newPassword); len(problems) > 0 {
		return httpx.Fail(http.StatusBadRequest, "%s", strings.Join(problems, " "))
	}
	if err := s.Store.SetPassword(ctx, u.ID, HashPassword(newPassword), uuid.NewString()); err != nil {
		return err
	}
	if err := s.Store.ResetFailures(ctx, u.ID); err != nil {
		return err
	}
	return s.Store.RevokeAll(ctx, u.ID)
}

// ChangePassword changes the password, ends every other session, and signs this one back in on a fresh token.
func (s *Service) ChangePassword(ctx context.Context, userID uuid.UUID, current, next string) (*Session, error) {
	u, err := s.Store.ByID(ctx, userID)
	if err != nil {
		return nil, httpx.Fail(http.StatusUnauthorized, "Sign in again.")
	}
	if current == "" || next == "" {
		return nil, httpx.Fail(http.StatusBadRequest, "The current and the new password are required.")
	}
	if err := requireReasonableLength(next); err != nil {
		return nil, err
	}
	if len(current) > MaxPasswordLength || !VerifyPassword(current, u.PasswordHash) {
		return nil, httpx.Fail(http.StatusBadRequest, "The current password is not correct.")
	}
	if problems := PasswordPolicy(next); len(problems) > 0 {
		return nil, httpx.Fail(http.StatusBadRequest, "%s", strings.Join(problems, " "))
	}
	if err := s.Store.SetPassword(ctx, u.ID, HashPassword(next), uuid.NewString()); err != nil {
		return nil, err
	}
	if err := s.Store.RevokeAll(ctx, u.ID); err != nil {
		return nil, err
	}
	u, err = s.Store.ByID(ctx, userID) // pick up the new session version before issuing under it
	if err != nil {
		return nil, err
	}
	return s.issue(ctx, u, uuid.New(), "")
}

// LogoutEverywhere ends every session of the caller on every device, including access tokens already sold.
func (s *Service) LogoutEverywhere(ctx context.Context, userID uuid.UUID) error {
	return s.Store.RevokeAll(ctx, userID)
}

// MfaSetup is the key to scan.
type MfaSetup struct {
	SharedKey  string `json:"sharedKey"`
	OtpAuthURI string `json:"otpAuthUri"`
}

// SetupMfa makes a new authenticator key. Two-factor stays off until a code proves the key was scanned.
func (s *Service) SetupMfa(ctx context.Context, userID uuid.UUID) (MfaSetup, error) {
	u, err := s.Store.ByID(ctx, userID)
	if err != nil {
		return MfaSetup{}, httpx.Fail(http.StatusUnauthorized, "Sign in again.")
	}
	if u.TwoFactorEnabled {
		return MfaSetup{}, httpx.Fail(http.StatusConflict, "Two-factor authentication is already on.")
	}
	secret := NewTOTPSecret()
	if err := s.Store.SetTOTPSecret(ctx, u.ID, secret); err != nil {
		return MfaSetup{}, err
	}
	return MfaSetup{SharedKey: secret, OtpAuthURI: OtpAuthURI(s.Opts.AppName, u.Email, secret)}, nil
}

// EnableMfa turns two-factor on once a code from the authenticator proves the key; it returns the one-time recovery codes.
func (s *Service) EnableMfa(ctx context.Context, userID uuid.UUID, code string) ([]string, error) {
	u, err := s.Store.ByID(ctx, userID)
	if err != nil {
		return nil, httpx.Fail(http.StatusUnauthorized, "Sign in again.")
	}
	if strings.TrimSpace(code) == "" || u.TOTPSecret == "" || !ValidTOTP(u.TOTPSecret, code, s.now()) {
		return nil, httpx.Fail(http.StatusBadRequest, "That code is not valid. Check the time on your phone and try again.")
	}
	codes := NewRecoveryCodes(8)
	if err := s.Store.EnableTwoFactor(ctx, u.ID, codes); err != nil {
		return nil, err
	}
	return codes, nil
}

// DisableMfa needs the password and a code, then ends every session.
func (s *Service) DisableMfa(ctx context.Context, userID uuid.UUID, password, code string) error {
	u, err := s.Store.ByID(ctx, userID)
	if err != nil {
		return httpx.Fail(http.StatusUnauthorized, "Sign in again.")
	}
	if password == "" || len(password) > MaxPasswordLength || !VerifyPassword(password, u.PasswordHash) {
		return httpx.Fail(http.StatusBadRequest, "The password is not correct.")
	}
	if u.TwoFactorEnabled {
		ok := false
		if strings.TrimSpace(code) != "" {
			if ok, err = s.verifyCode(ctx, u, code); err != nil {
				return err
			}
		}
		if !ok {
			return httpx.Fail(http.StatusBadRequest, "That code is not valid.")
		}
	}
	if err := s.Store.DisableTwoFactor(ctx, u.ID); err != nil {
		return err
	}
	return s.Store.RevokeAll(ctx, u.ID)
}

// MfaEnabled reports whether the user has two-factor on.
func (s *Service) MfaEnabled(ctx context.Context, userID uuid.UUID) (bool, error) {
	u, err := s.Store.ByID(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return u.TwoFactorEnabled, err
}

func (s *Service) sendConfirmation(ctx context.Context, u User) error {
	token := s.Tokens.PurposeToken(PurposeConfirmEmail, u, 3*24*time.Hour)
	link := fmt.Sprintf("%s/?confirm=%s&token=%s", strings.TrimRight(s.Opts.PublicURL, "/"), u.ID, token)
	return s.Mailer.Send(ctx, MailMessage{
		To: u.Email, Subject: fmt.Sprintf("Confirm your %s email address", s.Opts.AppName), Link: link,
		Body: fmt.Sprintf("Welcome. Open this link to confirm your email address.\n\n%s", link),
	})
}

// Refresh exchanges a refresh token for a new pair. The old token is revoked with one conditional UPDATE, so two requests
// presenting the same token cannot both succeed. A token that was already used means it leaked: the whole family is revoked.
func (s *Service) Refresh(ctx context.Context, raw string) (*Session, error) {
	unauthorised := httpx.Fail(http.StatusUnauthorized, "The session has expired. Sign in again.")
	if raw == "" {
		return nil, unauthorised
	}
	stored, err := s.Store.RefreshByHash(ctx, HashToken(raw))
	if errors.Is(err, ErrNotFound) {
		return nil, unauthorised
	}
	if err != nil {
		return nil, err
	}
	if stored.RevokedAt != nil {
		_ = s.Store.RevokeFamily(ctx, stored.FamilyID)
		return nil, unauthorised
	}
	if !stored.ExpiresAt.After(s.now()) {
		return nil, unauthorised
	}
	u, err := s.Store.ByID(ctx, stored.UserID)
	if err != nil || u.IsLockedOut(s.now()) {
		return nil, unauthorised
	}
	next := NewRefreshToken()
	won, err := s.Store.RotateRefresh(ctx, stored.ID, HashToken(next))
	if err != nil {
		return nil, err
	}
	if !won {
		_ = s.Store.RevokeFamily(ctx, stored.FamilyID)
		return nil, unauthorised
	}
	return s.issue(ctx, u, stored.FamilyID, next)
}

// Logout revokes the family of the presented refresh token.
func (s *Service) Logout(ctx context.Context, raw string) error {
	if raw == "" {
		return nil
	}
	stored, err := s.Store.RefreshByHash(ctx, HashToken(raw))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.Store.RevokeFamily(ctx, stored.FamilyID)
}

func (s *Service) issue(ctx context.Context, u User, family uuid.UUID, refresh string) (*Session, error) {
	access, err := s.Tokens.CreateAccessToken(Claims{UserID: u.ID, Email: u.Email, Roles: u.Roles, Version: u.SessionVersion})
	if err != nil {
		return nil, err
	}
	if refresh == "" {
		refresh = NewRefreshToken()
	}
	expires := s.now().Add(time.Duration(s.Opts.RefreshDays) * 24 * time.Hour)
	if err := s.Store.InsertRefresh(ctx, u.ID, family, HashToken(refresh), expires); err != nil {
		return nil, err
	}
	return &Session{Access: access, RefreshToken: refresh, RefreshExpiresAt: expires, User: UserInfo{ID: u.ID, Email: u.Email, Roles: u.Roles}}, nil
}

// Seed makes sure the first administrator exists, if one is configured. The three roles are plain strings, so nothing else is needed.
func (s *Service) Seed(ctx context.Context, email, password string) (created bool, err error) {
	if strings.TrimSpace(email) == "" || password == "" {
		return false, nil
	}
	if _, err := s.Store.ByEmail(ctx, email); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	id, _ := uuid.NewV7()
	u := User{ID: id, Email: email, PasswordHash: HashPassword(password), SecurityStamp: uuid.NewString(), EmailConfirmed: true}
	if err := s.Store.CreateUser(ctx, u, httpx.RoleAdmin); err != nil {
		return false, err
	}
	return true, nil
}
