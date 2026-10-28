// Package httpx holds what every module's HTTP layer shares: problem+json errors, JSON helpers, middleware and the
// signed-in principal.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// DomainError is an error the caller is told about: it carries the HTTP status and the title of the problem response.
type DomainError struct {
	Status  int
	Message string
}

func (e *DomainError) Error() string { return e.Message }

// Fail builds a DomainError.
func Fail(status int, format string, args ...any) *DomainError {
	return &DomainError{Status: status, Message: fmt.Sprintf(format, args...)}
}

// Problem is the body of an error response (RFC 9457 problem details).
type Problem struct {
	Title         string `json:"title"`
	Status        int    `json:"status"`
	CorrelationID string `json:"correlationId,omitempty"`
}

// HandlerFunc is a handler that returns its failure instead of writing it; Wrap turns the failure into problem+json.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Wrap adapts a HandlerFunc. A DomainError becomes its status; anything else is logged and becomes a 500.
func Wrap(log *slog.Logger, h HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			WriteError(log, w, r, err)
		}
	}
}

// WriteError writes the problem response for err.
func WriteError(log *slog.Logger, w http.ResponseWriter, r *http.Request, err error) {
	status, title := http.StatusInternalServerError, "An error occurred while processing your request."
	var d *DomainError
	if errors.As(err, &d) {
		status, title = d.Status, d.Message
	} else if r.Context().Err() == nil {
		log.Error("unhandled error", "method", r.Method, "path", r.URL.Path, "error", err, "correlationId", CorrelationOf(r.Context()))
	}
	WriteProblem(w, r, status, title)
}

// WriteProblem writes application/problem+json with the request's correlation id.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, title string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{Title: title, Status: status, CorrelationID: CorrelationOf(r.Context())})
}

// JSON writes v as JSON with the given status.
func JSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

// Decode reads a JSON body (at most 1 MiB). A malformed body is a 400.
func Decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // an empty body is the zero request; validation says what is missing
		}
		return Fail(http.StatusBadRequest, "The request body is not valid JSON.")
	}
	return nil
}

// ---- correlation id, security headers, panics, access log ----

type ctxKey int

const (
	correlationKey ctxKey = iota
	principalKey
)

// CorrelationHeader is echoed on every response and appears in every problem body.
const CorrelationHeader = "X-Correlation-Id"

// CorrelationOf returns the request's correlation id, or "".
func CorrelationOf(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey).(string)
	return id
}

// AcceptableCorrelationID: short, printable, no spaces. Arbitrary caller input is never echoed into headers and logs.
func AcceptableCorrelationID(v string) bool {
	if len(v) == 0 || len(v) > 64 {
		return false
	}
	for _, c := range v {
		if !(unicode.IsLetter(c) || unicode.IsDigit(c) || c == '-' || c == '_') || c > unicode.MaxASCII {
			return false
		}
	}
	return true
}

// Correlation takes the caller's correlation id (or makes one) and echoes it.
func Correlation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(CorrelationHeader)
		if !AcceptableCorrelationID(id) {
			b := make([]byte, 16)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set(CorrelationHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), correlationKey, id)))
	})
}

// SecurityHeaders sets the headers every response carries.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api") {
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

// Recover turns a panic into a logged 500 problem response.
func Recover(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Error("panic", "path", r.URL.Path, "panic", fmt.Sprint(v), "stack", string(debug.Stack()), "correlationId", CorrelationOf(r.Context()))
				WriteProblem(w, r, http.StatusInternalServerError, "An error occurred while processing your request.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }

// AccessLog logs one line per request, except the probes.
func AccessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/api/health") {
			return
		}
		log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "correlationId", CorrelationOf(r.Context()))
	})
}

// ---- the signed-in principal ----

// Roles.
const (
	RoleAdmin    = "Admin"
	RoleStaff    = "Staff"
	RoleCustomer = "Customer"
)

// Principal is who the access token says is calling.
type Principal struct {
	ID    uuid.UUID
	Email string
	Roles []string
}

// IsStaff is true for Admin and Staff.
func (p *Principal) IsStaff() bool {
	if p == nil {
		return false
	}
	for _, r := range p.Roles {
		if r == RoleAdmin || r == RoleStaff {
			return true
		}
	}
	return false
}

// CanAccess is the owner-or-staff rule every per-user resource follows.
func (p *Principal) CanAccess(owner uuid.UUID) bool {
	return p != nil && (p.IsStaff() || p.ID == owner)
}

// PrincipalOf returns the signed-in principal, or nil.
func PrincipalOf(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey).(*Principal)
	return p
}

// Authenticator turns a bearer token into a principal. It rejects tampered, expired and revoked tokens.
type Authenticator interface {
	Authenticate(ctx context.Context, bearer string) (*Principal, error)
}

// Authenticated requires a valid bearer token. Everything is authenticated unless a route is registered without this.
func Authenticated(log *slog.Logger, auth Authenticator, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || strings.TrimSpace(token) == "" {
			WriteProblem(w, r, http.StatusUnauthorized, "Sign in to continue.")
			return
		}
		p, err := auth.Authenticate(r.Context(), strings.TrimSpace(token))
		if err != nil || p == nil {
			if err != nil && r.Context().Err() == nil && !errors.Is(err, ErrInvalidToken) {
				log.Warn("authentication failed", "error", err)
			}
			WriteProblem(w, r, http.StatusUnauthorized, "Sign in to continue.")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	}
}

// ErrInvalidToken is returned by an Authenticator for any token that must be refused.
var ErrInvalidToken = errors.New("invalid token")

// Staff additionally requires the Admin or Staff role (403 otherwise). Use it inside Authenticated.
func Staff(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !PrincipalOf(r.Context()).IsStaff() {
			WriteProblem(w, r, http.StatusForbidden, "You do not have access to this.")
			return
		}
		next(w, r)
	}
}

// WithPrincipal puts a principal into a context (used by tests).
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}
