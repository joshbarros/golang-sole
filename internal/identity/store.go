package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// User is a row of identity.users.
type User struct {
	ID               uuid.UUID
	Email            string
	PasswordHash     string
	SecurityStamp    string
	EmailConfirmed   bool
	TwoFactorEnabled bool
	TOTPSecret       string
	LockoutEnd       *time.Time
	SessionVersion   int
	Roles            []string
}

// RefreshToken is a stored refresh token (only its hash).
type RefreshToken struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	FamilyID  uuid.UUID
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// Store is the identity module's database access.
type Store struct{ Pool *pgxpool.Pool }

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrDuplicateEmail is returned when the email is taken.
var ErrDuplicateEmail = errors.New("duplicate email")

const userColumns = `id, email, password_hash, security_stamp, email_confirmed, two_factor_enabled, COALESCE(totp_secret,''), lockout_end, session_version`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.SecurityStamp, &u.EmailConfirmed, &u.TwoFactorEnabled, &u.TOTPSecret, &u.LockoutEnd, &u.SessionVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

func normalise(email string) string { return strings.ToUpper(strings.TrimSpace(email)) }

// CreateUser inserts a user and its role.
func (s *Store) CreateUser(ctx context.Context, u User, role string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	_, err = tx.Exec(ctx, `INSERT INTO identity.users (id, email, normalized_email, password_hash, security_stamp, email_confirmed) VALUES ($1,$2,$3,$4,$5,$6)`,
		u.ID, u.Email, normalise(u.Email), u.PasswordHash, u.SecurityStamp, u.EmailConfirmed)
	if err != nil {
		if isUnique(err) {
			return ErrDuplicateEmail
		}
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity.user_roles (user_id, role) VALUES ($1,$2)`, u.ID, role); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func isUnique(err error) bool {
	var coder interface{ SQLState() string }
	return errors.As(err, &coder) && coder.SQLState() == "23505"
}

// ByEmail finds a user by (case-insensitive) email.
func (s *Store) ByEmail(ctx context.Context, email string) (User, error) {
	u, err := scanUser(s.Pool.QueryRow(ctx, `SELECT `+userColumns+` FROM identity.users WHERE normalized_email = $1`, normalise(email)))
	if err != nil {
		return u, err
	}
	u.Roles, err = s.roles(ctx, u.ID)
	return u, err
}

// ByID finds a user by id.
func (s *Store) ByID(ctx context.Context, id uuid.UUID) (User, error) {
	u, err := scanUser(s.Pool.QueryRow(ctx, `SELECT `+userColumns+` FROM identity.users WHERE id = $1`, id))
	if err != nil {
		return u, err
	}
	u.Roles, err = s.roles(ctx, u.ID)
	return u, err
}

func (s *Store) roles(ctx context.Context, id uuid.UUID) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT role FROM identity.user_roles WHERE user_id = $1 ORDER BY role`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	roles := []string{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		roles = append(roles, r)
	}
	return roles, rows.Err()
}

// AnyAdmin reports whether an administrator exists.
func (s *Store) AnyAdmin(ctx context.Context) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identity.user_roles WHERE role = 'Admin')`).Scan(&ok)
	return ok, err
}

// IsLockedOut is true while the lockout lasts.
func (u User) IsLockedOut(now time.Time) bool { return u.LockoutEnd != nil && u.LockoutEnd.After(now) }

// RegisterFailure counts one failed attempt; the fifth locks the account for 15 minutes. Returns whether it is now locked.
func (s *Store) RegisterFailure(ctx context.Context, id uuid.UUID) (bool, error) {
	var locked bool
	err := s.Pool.QueryRow(ctx, `
		UPDATE identity.users SET
		    access_failed_count = CASE WHEN access_failed_count + 1 >= 5 THEN 0 ELSE access_failed_count + 1 END,
		    lockout_end = CASE WHEN access_failed_count + 1 >= 5 THEN now() + interval '15 minutes' ELSE lockout_end END
		WHERE id = $1
		RETURNING lockout_end IS NOT NULL AND lockout_end > now()`, id).Scan(&locked)
	return locked, err
}

// ResetFailures clears the failed-attempt count and any lockout.
func (s *Store) ResetFailures(ctx context.Context, id uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `UPDATE identity.users SET access_failed_count = 0, lockout_end = NULL WHERE id = $1`, id)
	return err
}

// ConfirmEmail marks the address confirmed.
func (s *Store) ConfirmEmail(ctx context.Context, id uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `UPDATE identity.users SET email_confirmed = true WHERE id = $1`, id)
	return err
}

// SetPassword stores a new hash and rotates the security stamp (which kills any reset link issued before).
func (s *Store) SetPassword(ctx context.Context, id uuid.UUID, hash, stamp string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE identity.users SET password_hash = $2, security_stamp = $3 WHERE id = $1`, id, hash, stamp)
	return err
}

// RevokeAll revokes every refresh token of the user and, through one atomic increment of the session version, every
// access token sold so far.
func (s *Store) RevokeAll(ctx context.Context, id uuid.UUID) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `UPDATE identity.refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE identity.users SET session_version = session_version + 1 WHERE id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SessionVersion reads the user's current session version (ErrNotFound for an unknown user).
func (s *Store) SessionVersion(ctx context.Context, id uuid.UUID) (int, error) {
	var v int
	err := s.Pool.QueryRow(ctx, `SELECT session_version FROM identity.users WHERE id = $1`, id).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return v, err
}

// ---- refresh tokens ----

// InsertRefresh stores a refresh token by its hash.
func (s *Store) InsertRefresh(ctx context.Context, userID, familyID uuid.UUID, hash string, expires time.Time) error {
	id, _ := uuid.NewV7()
	_, err := s.Pool.Exec(ctx, `INSERT INTO identity.refresh_tokens (id, user_id, family_id, token_hash, created_at, expires_at) VALUES ($1,$2,$3,$4,now(),$5)`,
		id, userID, familyID, hash, expires)
	return err
}

// RefreshByHash finds a stored refresh token.
func (s *Store) RefreshByHash(ctx context.Context, hash string) (RefreshToken, error) {
	var t RefreshToken
	err := s.Pool.QueryRow(ctx, `SELECT id, user_id, family_id, expires_at, revoked_at FROM identity.refresh_tokens WHERE token_hash = $1`, hash).
		Scan(&t.ID, &t.UserID, &t.FamilyID, &t.ExpiresAt, &t.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// RotateRefresh revokes a token with one conditional UPDATE, so two requests presenting the same token cannot both win.
func (s *Store) RotateRefresh(ctx context.Context, id uuid.UUID, nextHash string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE identity.refresh_tokens SET revoked_at = now(), replaced_by_hash = $2 WHERE id = $1 AND revoked_at IS NULL`, id, nextHash)
	return tag.RowsAffected() == 1, err
}

// RevokeFamily revokes every live token of a family.
func (s *Store) RevokeFamily(ctx context.Context, family uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `UPDATE identity.refresh_tokens SET revoked_at = now() WHERE family_id = $1 AND revoked_at IS NULL`, family)
	return err
}

// ---- two-factor ----

// SetTOTPSecret stores a new authenticator key (two-factor stays off until it is proven).
func (s *Store) SetTOTPSecret(ctx context.Context, id uuid.UUID, secret string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE identity.users SET totp_secret = $2 WHERE id = $1`, id, secret)
	return err
}

// EnableTwoFactor turns two-factor on and replaces the recovery codes, in one transaction.
func (s *Store) EnableTwoFactor(ctx context.Context, id uuid.UUID, codes []string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `UPDATE identity.users SET two_factor_enabled = true WHERE id = $1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM identity.recovery_codes WHERE user_id = $1`, id); err != nil {
		return err
	}
	for _, c := range codes {
		if _, err := tx.Exec(ctx, `INSERT INTO identity.recovery_codes (user_id, code_hash) VALUES ($1,$2)`, id, HashToken(normaliseCode(c))); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// DisableTwoFactor turns it off and forgets the key and the codes.
func (s *Store) DisableTwoFactor(ctx context.Context, id uuid.UUID) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `UPDATE identity.users SET two_factor_enabled = false, totp_secret = NULL WHERE id = $1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM identity.recovery_codes WHERE user_id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func normaliseCode(c string) string { return strings.ToLower(strings.TrimSpace(c)) }

// RedeemRecoveryCode spends a one-time code; the conditional UPDATE makes it work once.
func (s *Store) RedeemRecoveryCode(ctx context.Context, id uuid.UUID, code string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE identity.recovery_codes SET used_at = now() WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL`, id, HashToken(normaliseCode(code)))
	return tag.RowsAffected() == 1, err
}

// ---- rate limit ----

// Hit counts one request for key in the minute window and returns the count so far.
func (s *Store) Hit(ctx context.Context, key string, window time.Time) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO identity.rate_limit_hits (key, window_start, count) VALUES ($1,$2,1)
		ON CONFLICT (key, window_start) DO UPDATE SET count = identity.rate_limit_hits.count + 1
		RETURNING count`, key, window).Scan(&n)
	return n, err
}

// SweepHits deletes windows older than the cutoff.
func (s *Store) SweepHits(ctx context.Context, cutoff time.Time) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM identity.rate_limit_hits WHERE window_start < $1`, cutoff)
	return err
}

func (u User) String() string { return fmt.Sprintf("user %s", u.ID) }
