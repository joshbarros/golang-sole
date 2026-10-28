package identity

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-sole/internal/platform/httpx"
)

// Cookie and header names the web app relies on.
const (
	RefreshCookie       = "sole_refresh"
	RequestedWithHeader = "X-Requested-With"
	RequestedWithValue  = "sole-web"
)

// HTTPOptions are the HTTP layer's settings.
type HTTPOptions struct {
	SecureCookies          bool
	RevocationCacheSeconds int
	AuthPerMinute          int
	TrustForwarded         bool
	DevMailbox             Mailbox // nil unless the dev mailbox is on
}

// API is the HTTP face of the identity module.
type API struct {
	Svc  *Service
	Opts HTTPOptions
	Log  *slog.Logger

	cacheMu sync.Mutex
	cache   map[uuid.UUID]versionEntry
}

type versionEntry struct {
	version int
	expires time.Time
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

type tokenResponse struct {
	AccessToken string    `json:"accessToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
	User        UserInfo  `json:"user"`
}

type message struct {
	Message string `json:"message"`
}

func toResponse(s *Session) tokenResponse {
	return tokenResponse{AccessToken: s.Access.Value, ExpiresAt: s.Access.ExpiresAt, User: s.User}
}

// Authenticate implements httpx.Authenticator: the token must verify, and its session version must still be current. A
// password change, a reset or "sign out everywhere" bumps the version; older tokens stop here. The version is cached
// briefly so this is not a database read per request.
func (a *API) Authenticate(ctx context.Context, bearer string) (*httpx.Principal, error) {
	c, err := a.Svc.Tokens.Parse(bearer)
	if err != nil {
		return nil, httpx.ErrInvalidToken
	}
	current, err := a.currentVersion(ctx, c.UserID)
	if err != nil {
		if err == ErrNotFound {
			return nil, httpx.ErrInvalidToken
		}
		return nil, err
	}
	if current != c.Version {
		return nil, httpx.ErrInvalidToken
	}
	return &httpx.Principal{ID: c.UserID, Email: c.Email, Roles: c.Roles}, nil
}

func (a *API) currentVersion(ctx context.Context, id uuid.UUID) (int, error) {
	ttl := time.Duration(a.Opts.RevocationCacheSeconds) * time.Second
	now := time.Now()
	if ttl > 0 {
		a.cacheMu.Lock()
		if e, ok := a.cache[id]; ok && e.expires.After(now) {
			a.cacheMu.Unlock()
			return e.version, nil
		}
		a.cacheMu.Unlock()
	}
	v, err := a.Svc.Store.SessionVersion(ctx, id)
	if err != nil {
		return 0, err
	}
	if ttl > 0 {
		a.cacheMu.Lock()
		if a.cache == nil {
			a.cache = map[uuid.UUID]versionEntry{}
		}
		if len(a.cache) > 10_000 {
			a.cache = map[uuid.UUID]versionEntry{}
		}
		a.cache[id] = versionEntry{version: v, expires: now.Add(ttl)}
		a.cacheMu.Unlock()
	}
	return v, nil
}

// Routes registers the identity endpoints. Everything except register, login, the email flows, refresh and logout needs a token.
func (a *API) Routes(mux *http.ServeMux) {
	w := func(h httpx.HandlerFunc) http.HandlerFunc { return httpx.Wrap(a.Log, h) }
	authed := func(h httpx.HandlerFunc) http.HandlerFunc { return httpx.Authenticated(a.Log, a, w(h)) }

	mux.HandleFunc("POST /api/auth/register", w(a.register))
	mux.HandleFunc("POST /api/auth/login", w(a.login))
	mux.HandleFunc("POST /api/auth/confirm-email", w(a.confirmEmail))
	mux.HandleFunc("POST /api/auth/resend-confirmation", w(a.resendConfirmation))
	mux.HandleFunc("POST /api/auth/forgot-password", w(a.forgotPassword))
	mux.HandleFunc("POST /api/auth/reset-password", w(a.resetPassword))
	mux.HandleFunc("POST /api/auth/refresh", w(a.refresh))
	mux.HandleFunc("POST /api/auth/logout", w(a.logout))
	mux.HandleFunc("POST /api/auth/change-password", authed(a.changePassword))
	mux.HandleFunc("POST /api/auth/logout-everywhere", authed(a.logoutEverywhere))
	mux.HandleFunc("GET /api/auth/mfa", authed(a.mfa))
	mux.HandleFunc("POST /api/auth/mfa/setup", authed(a.mfaSetup))
	mux.HandleFunc("POST /api/auth/mfa/enable", authed(a.mfaEnable))
	mux.HandleFunc("POST /api/auth/mfa/disable", authed(a.mfaDisable))
	mux.HandleFunc("GET /api/auth/me", authed(a.me))
	mux.HandleFunc("GET /.well-known/jwks.json", w(a.jwks))
	mux.HandleFunc("GET /api/dev/mailbox", w(a.devMailbox))
}

func (a *API) register(rw http.ResponseWriter, r *http.Request) error {
	var req credentials
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	s, err := a.Svc.Register(r.Context(), req.Email, req.Password)
	if err != nil {
		return err
	}
	if s == nil {
		return httpx.JSON(rw, http.StatusAccepted, message{"Check your inbox to confirm your email address, then sign in."})
	}
	a.setCookie(rw, r, s)
	return httpx.JSON(rw, http.StatusCreated, toResponse(s))
}

func (a *API) login(rw http.ResponseWriter, r *http.Request) error {
	var req credentials
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	s, err := a.Svc.Login(r.Context(), req.Email, req.Password, req.Code)
	if err != nil {
		return err
	}
	a.setCookie(rw, r, s)
	return httpx.JSON(rw, http.StatusOK, toResponse(s))
}

func (a *API) confirmEmail(rw http.ResponseWriter, r *http.Request) error {
	var req struct {
		UserID uuid.UUID `json:"userId"`
		Token  string    `json:"token"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if err := a.Svc.ConfirmEmail(r.Context(), req.UserID, req.Token); err != nil {
		return err
	}
	rw.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) resendConfirmation(rw http.ResponseWriter, r *http.Request) error {
	var req credentials
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if err := a.Svc.ResendConfirmation(r.Context(), req.Email); err != nil {
		return err
	}
	return httpx.JSON(rw, http.StatusAccepted, message{"If that address needs confirming, a new link is on its way."})
}

func (a *API) forgotPassword(rw http.ResponseWriter, r *http.Request) error {
	var req credentials
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if err := a.Svc.ForgotPassword(r.Context(), req.Email); err != nil {
		return err
	}
	return httpx.JSON(rw, http.StatusAccepted, message{"If that address has an account, a reset link is on its way."})
}

func (a *API) resetPassword(rw http.ResponseWriter, r *http.Request) error {
	var req struct {
		UserID      uuid.UUID `json:"userId"`
		Token       string    `json:"token"`
		NewPassword string    `json:"newPassword"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if err := a.Svc.ResetPassword(r.Context(), req.UserID, req.Token, req.NewPassword); err != nil {
		return err
	}
	rw.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) changePassword(rw http.ResponseWriter, r *http.Request) error {
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	s, err := a.Svc.ChangePassword(r.Context(), httpx.PrincipalOf(r.Context()).ID, req.CurrentPassword, req.NewPassword)
	if err != nil {
		return err
	}
	a.invalidateCache(httpx.PrincipalOf(r.Context()).ID)
	a.setCookie(rw, r, s)
	return httpx.JSON(rw, http.StatusOK, toResponse(s))
}

func (a *API) logoutEverywhere(rw http.ResponseWriter, r *http.Request) error {
	id := httpx.PrincipalOf(r.Context()).ID
	if err := a.Svc.LogoutEverywhere(r.Context(), id); err != nil {
		return err
	}
	a.invalidateCache(id)
	a.clearCookie(rw, r)
	rw.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) mfa(rw http.ResponseWriter, r *http.Request) error {
	on, err := a.Svc.MfaEnabled(r.Context(), httpx.PrincipalOf(r.Context()).ID)
	if err != nil {
		return err
	}
	return httpx.JSON(rw, http.StatusOK, map[string]bool{"enabled": on})
}

func (a *API) mfaSetup(rw http.ResponseWriter, r *http.Request) error {
	setup, err := a.Svc.SetupMfa(r.Context(), httpx.PrincipalOf(r.Context()).ID)
	if err != nil {
		return err
	}
	return httpx.JSON(rw, http.StatusOK, setup)
}

func (a *API) mfaEnable(rw http.ResponseWriter, r *http.Request) error {
	var req struct {
		Code string `json:"code"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	codes, err := a.Svc.EnableMfa(r.Context(), httpx.PrincipalOf(r.Context()).ID, req.Code)
	if err != nil {
		return err
	}
	return httpx.JSON(rw, http.StatusOK, map[string][]string{"codes": codes})
}

func (a *API) mfaDisable(rw http.ResponseWriter, r *http.Request) error {
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	id := httpx.PrincipalOf(r.Context()).ID
	if err := a.Svc.DisableMfa(r.Context(), id, req.Password, req.Code); err != nil {
		return err
	}
	a.invalidateCache(id)
	a.clearCookie(rw, r)
	rw.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) me(rw http.ResponseWriter, r *http.Request) error {
	p := httpx.PrincipalOf(r.Context())
	return httpx.JSON(rw, http.StatusOK, UserInfo{ID: p.ID, Email: p.Email, Roles: p.Roles})
}

// requireSameSite: the refresh token travels in an HttpOnly cookie. A custom header is required so another site cannot
// make the browser call refresh or logout.
func requireSameSite(r *http.Request) error {
	if r.Header.Get(RequestedWithHeader) != RequestedWithValue {
		return httpx.Fail(http.StatusBadRequest, "The %s header is required.", RequestedWithHeader)
	}
	return nil
}

func cookieValue(r *http.Request) string {
	c, err := r.Cookie(RefreshCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func (a *API) refresh(rw http.ResponseWriter, r *http.Request) error {
	if err := requireSameSite(r); err != nil {
		return err
	}
	s, err := a.Svc.Refresh(r.Context(), cookieValue(r))
	if err != nil {
		return err
	}
	a.setCookie(rw, r, s)
	return httpx.JSON(rw, http.StatusOK, toResponse(s))
}

func (a *API) logout(rw http.ResponseWriter, r *http.Request) error {
	if err := requireSameSite(r); err != nil {
		return err
	}
	if err := a.Svc.Logout(r.Context(), cookieValue(r)); err != nil {
		return err
	}
	a.clearCookie(rw, r)
	rw.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) jwks(rw http.ResponseWriter, r *http.Request) error {
	set := a.Svc.Tokens.JWKS()
	if set == nil {
		return httpx.Fail(http.StatusNotFound, "Tokens are signed with a shared secret; there is no public key.")
	}
	return httpx.JSON(rw, http.StatusOK, set)
}

func (a *API) devMailbox(rw http.ResponseWriter, r *http.Request) error {
	if a.Opts.DevMailbox == nil {
		return httpx.Fail(http.StatusNotFound, "Not found.")
	}
	msgs, err := a.Opts.DevMailbox.For(r.Context(), r.URL.Query().Get("to"))
	if err != nil {
		return err
	}
	return httpx.JSON(rw, http.StatusOK, msgs)
}

func (a *API) secure(r *http.Request) bool {
	return a.Opts.SecureCookies || r.TLS != nil || (a.Opts.TrustForwarded && r.Header.Get("X-Forwarded-Proto") == "https")
}

func (a *API) setCookie(rw http.ResponseWriter, r *http.Request, s *Session) {
	http.SetCookie(rw, &http.Cookie{
		Name: RefreshCookie, Value: s.RefreshToken, Path: "/api/auth", Expires: s.RefreshExpiresAt,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: a.secure(r),
	})
}

func (a *API) clearCookie(rw http.ResponseWriter, r *http.Request) {
	http.SetCookie(rw, &http.Cookie{
		Name: RefreshCookie, Value: "", Path: "/api/auth", MaxAge: -1, Expires: time.Unix(0, 0),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: a.secure(r),
	})
}

func (a *API) invalidateCache(id uuid.UUID) {
	a.cacheMu.Lock()
	delete(a.cache, id)
	a.cacheMu.Unlock()
}

// ClientKey is the address a request is counted under: the connection's, or with forwarded headers trusted, the first hop.
func ClientKey(r *http.Request, trustForwarded bool) string {
	if trustForwarded {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if first = strings.TrimSpace(first); first != "" && len(first) <= 64 {
				return first
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host == "" {
		return "unknown"
	}
	return host
}

// RateLimit is a fixed-window limit on /api/auth/*, counted in Postgres so every API replica shares one count per client.
// One upsert per request; windows older than an hour are swept now and then.
func (a *API) RateLimit(next http.Handler) http.Handler {
	var lastSweep atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/auth") || a.Opts.AuthPerMinute <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		now := time.Now().UTC()
		window := now.Truncate(time.Minute)
		count, err := a.Svc.Store.Hit(r.Context(), ClientKey(r, a.Opts.TrustForwarded), window)
		if err != nil {
			httpx.WriteError(a.Log, w, r, err)
			return
		}
		if count > a.Opts.AuthPerMinute {
			w.Header().Set("Retry-After", strconv.Itoa(60-now.Second()))
			httpx.WriteProblem(w, r, http.StatusTooManyRequests, "Too many requests. Try again in a minute.")
			return
		}
		tick := now.Unix() / 60
		if last := lastSweep.Load(); tick-last >= 30 && lastSweep.CompareAndSwap(last, tick) {
			_ = a.Svc.Store.SweepHits(r.Context(), now.Add(-time.Hour))
		}
		next.ServeHTTP(w, r)
	})
}
