package integration

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joshbarros/golang-sole/internal/identity"
)

func TestRegisterSignsInAndSetsAnHttpOnlyStrictCookie(t *testing.T) {
	e := newEnv(t)
	r := e.post("/api/auth/register", map[string]any{"email": newEmail(), "password": password}, "")
	if r.Status != 201 || r.str("accessToken") == "" || r.obj("user")["roles"].([]any)[0] != "Customer" {
		t.Fatalf("register: %d %s", r.Status, r.Raw)
	}
	var c *http.Cookie
	for _, k := range r.Cookies {
		if k.Name == "sole_refresh" {
			c = k
		}
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/api/auth" || c.Value == "" {
		t.Fatalf("the refresh cookie must be HttpOnly, SameSite=Strict, scoped to /api/auth: %+v", c)
	}
}

func TestRegisterRefusals(t *testing.T) {
	e := newEnv(t)
	email := newEmail()
	if r := e.post("/api/auth/register", map[string]any{"email": email, "password": password}, ""); r.Status != 201 {
		t.Fatal(r.Status)
	}
	cases := map[string]struct {
		body   map[string]any
		status int
		has    string
	}{
		"taken (any case)": {map[string]any{"email": strings.ToUpper(email), "password": password}, 409, "already taken"},
		"weak":             {map[string]any{"email": newEmail(), "password": "short"}, 400, "at least 10"},
		"no digit":         {map[string]any{"email": newEmail(), "password": "Abcdefghijk"}, 400, "digit"},
		"overlong":         {map[string]any{"email": newEmail(), "password": "Aa1" + strings.Repeat("x", 200)}, 400, "at most 128"},
		"bad email":        {map[string]any{"email": "not-an-email", "password": password}, 400, "valid email"},
		"no password":      {map[string]any{"email": newEmail()}, 400, "password is required"},
	}
	for name, c := range cases {
		r := e.post("/api/auth/register", c.body, "")
		if r.Status != c.status || !strings.Contains(r.str("title"), c.has) {
			t.Errorf("%s: %d %s", name, r.Status, r.Raw)
		}
	}
}

func TestLoginAnswersTheSameForAWrongPasswordAndAnUnknownUser(t *testing.T) {
	e := newEnv(t)
	u := e.register()
	wrong := e.post("/api/auth/login", map[string]any{"email": u.Email, "password": "Wrong-Passw0rd"}, "")
	unknown := e.post("/api/auth/login", map[string]any{"email": newEmail(), "password": password}, "")
	if wrong.Status != 401 || unknown.Status != 401 || wrong.str("title") != unknown.str("title") {
		t.Fatalf("a wrong password and an unknown user must be indistinguishable: %d %s / %d %s", wrong.Status, wrong.Raw, unknown.Status, unknown.Raw)
	}
	if r := e.post("/api/auth/login", map[string]any{"email": u.Email, "password": strings.Repeat("a", 500)}, ""); r.Status != 401 {
		t.Fatalf("an absurdly long password is refused before hashing: %d", r.Status)
	}
}

func TestFiveWrongPasswordsLockTheAccountEvenForTheRightOne(t *testing.T) {
	e := newEnv(t)
	u := e.register()
	var last reply
	for i := 0; i < 5; i++ {
		last = e.post("/api/auth/login", map[string]any{"email": u.Email, "password": "Wrong-Passw0rd"}, "")
	}
	if last.Status != 429 {
		t.Fatalf("the fifth failure locks: %d %s", last.Status, last.Raw)
	}
	if r := e.post("/api/auth/login", map[string]any{"email": u.Email, "password": password}, ""); r.Status != 429 {
		t.Fatalf("a locked account refuses even the right password: %d", r.Status)
	}
}

func TestMeAndTheTokensItRefuses(t *testing.T) {
	e := newEnv(t)
	u := e.register()
	if r := e.get("/api/auth/me", u.Token); r.Status != 200 || r.str("email") != u.Email {
		t.Fatalf("me: %d %s", r.Status, r.Raw)
	}
	parts := strings.Split(u.Token, ".")
	for name, tok := range map[string]string{"none": "", "garbage": "abc", "tampered": parts[0] + "." + parts[1] + ".AAAA", "other key": makeForeignToken(t)} {
		if r := e.get("/api/auth/me", tok); r.Status != 401 {
			t.Errorf("%s must be 401, got %d", name, r.Status)
		}
	}
}

func makeForeignToken(t *testing.T) string {
	t.Helper()
	svc, _ := identity.NewTokenService(identity.TokenConfig{Key: "another-signing-key-0123456789abcdef-xyz", Issuer: "sole-api", Audience: "sole-web", AccessMinutes: 15})
	tok, err := svc.CreateAccessToken(identity.Claims{UserID: [16]byte{1}, Email: "x@y.z", Roles: []string{"Admin"}})
	if err != nil {
		t.Fatal(err)
	}
	return tok.Value
}

func refreshCall(e *env, cookie string, withHeader bool) reply {
	h := map[string]string{}
	if withHeader {
		h["X-Requested-With"] = "sole-web"
	}
	return e.do(call{method: "POST", path: "/api/auth/refresh", cookie: cookie, headers: h})
}

func TestRefreshRotatesAndAReplayRevokesTheWholeFamily(t *testing.T) {
	e := newEnv(t)
	u := e.register()

	first := refreshCall(e, u.Refresh, true)
	if first.Status != 200 {
		t.Fatalf("refresh: %d %s", first.Status, first.Raw)
	}
	next := sessionOf(t, first, u.Email)
	if next.Refresh == "" || next.Refresh == u.Refresh {
		t.Fatal("the refresh token must rotate")
	}
	// The old token was already used: this is a replay, so it fails and takes the new token down with it.
	if r := refreshCall(e, u.Refresh, true); r.Status != 401 {
		t.Fatalf("a replayed token must be refused: %d", r.Status)
	}
	if r := refreshCall(e, next.Refresh, true); r.Status != 401 {
		t.Fatalf("the replay revokes the family, so the new token dies too: %d", r.Status)
	}
}

func TestRefreshNeedsTheHeaderAndAToken(t *testing.T) {
	e := newEnv(t)
	u := e.register()
	if r := refreshCall(e, u.Refresh, false); r.Status != 400 {
		t.Fatalf("without X-Requested-With another site could trigger it: %d", r.Status)
	}
	if r := refreshCall(e, "", true); r.Status != 401 {
		t.Fatalf("no cookie: %d", r.Status)
	}
	if r := refreshCall(e, "made-up", true); r.Status != 401 {
		t.Fatalf("unknown token: %d", r.Status)
	}
}

func TestTwoConcurrentRefreshesWithTheSameTokenExactlyOneWins(t *testing.T) {
	e := newEnv(t)
	u := e.register()
	var wg sync.WaitGroup
	results := make([]int, 6)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = refreshCall(e, u.Refresh, true).Status
		}()
	}
	wg.Wait()
	wins := 0
	for _, s := range results {
		if s == 200 {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one refresh may win, got %d: %v", wins, results)
	}
}

func TestLogoutEndsTheRefreshToken(t *testing.T) {
	e := newEnv(t)
	u := e.register()
	r := e.do(call{method: "POST", path: "/api/auth/logout", cookie: u.Refresh, headers: map[string]string{"X-Requested-With": "sole-web"}})
	if r.Status != 204 {
		t.Fatalf("logout: %d", r.Status)
	}
	if r := refreshCall(e, u.Refresh, true); r.Status != 401 {
		t.Fatalf("a logged-out token must not refresh: %d", r.Status)
	}
	if r := e.do(call{method: "POST", path: "/api/auth/logout", cookie: u.Refresh}); r.Status != 400 {
		t.Fatalf("logout needs the header too: %d", r.Status)
	}
}

func TestChangePasswordEndsOtherSessionsAndSignsThisOneBackIn(t *testing.T) {
	e := newEnv(t)
	u := e.register()
	other := sessionOf(t, e.post("/api/auth/login", map[string]any{"email": u.Email, "password": password}, ""), u.Email)

	if r := e.post("/api/auth/change-password", map[string]any{"currentPassword": "wrong", "newPassword": "New-Passw0rd-1"}, u.Token); r.Status != 400 {
		t.Fatalf("a wrong current password: %d", r.Status)
	}
	r := e.post("/api/auth/change-password", map[string]any{"currentPassword": password, "newPassword": "New-Passw0rd-1"}, u.Token)
	fresh := sessionOf(t, r, u.Email)
	if e.get("/api/auth/me", u.Token).Status != 401 || e.get("/api/auth/me", other.Token).Status != 401 {
		t.Fatal("every access token sold before the change must stop working")
	}
	if e.get("/api/auth/me", fresh.Token).Status != 200 {
		t.Fatal("the session that changed the password gets a fresh token")
	}
	if refreshCall(e, other.Refresh, true).Status != 401 {
		t.Fatal("the other session's refresh token is revoked")
	}
	if e.post("/api/auth/login", map[string]any{"email": u.Email, "password": password}, "").Status != 401 ||
		e.post("/api/auth/login", map[string]any{"email": u.Email, "password": "New-Passw0rd-1"}, "").Status != 200 {
		t.Fatal("only the new password signs in")
	}
}

func TestLogoutEverywhere(t *testing.T) {
	e := newEnv(t)
	u := e.register()
	other := sessionOf(t, e.post("/api/auth/login", map[string]any{"email": u.Email, "password": password}, ""), u.Email)
	if r := e.post("/api/auth/logout-everywhere", nil, u.Token); r.Status != 204 {
		t.Fatalf("logout-everywhere: %d", r.Status)
	}
	if e.get("/api/auth/me", u.Token).Status != 401 || e.get("/api/auth/me", other.Token).Status != 401 || refreshCall(e, other.Refresh, true).Status != 401 {
		t.Fatal("every session of the user must be over")
	}
}

func linkParam(t *testing.T, link, key string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get(key)
}

func lastMail(t *testing.T, e *env, to string) identity.MailMessage {
	t.Helper()
	msgs, err := e.app.Mailbox.For(context.Background(), to)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatalf("no mail for %s", to)
	}
	return msgs[0]
}

func TestEmailConfirmationGatesSignInAndTheLinkWorksOnce(t *testing.T) {
	e := newEnv(t, withConfirmedEmail())
	email := newEmail()
	r := e.post("/api/auth/register", map[string]any{"email": email, "password": password}, "")
	if r.Status != 202 || r.str("accessToken") != "" {
		t.Fatalf("with confirmation required, register gives no session: %d %s", r.Status, r.Raw)
	}
	if r := e.post("/api/auth/login", map[string]any{"email": email, "password": password}, ""); r.Status != 403 {
		t.Fatalf("sign-in waits for the confirmation: %d", r.Status)
	}
	link := lastMail(t, e, email).Link
	body := map[string]any{"userId": linkParam(t, link, "confirm"), "token": linkParam(t, link, "token")}
	if r := e.post("/api/auth/confirm-email", body, ""); r.Status != 204 {
		t.Fatalf("confirm: %d %s", r.Status, r.Raw)
	}
	if r := e.post("/api/auth/confirm-email", body, ""); r.Status != 400 {
		t.Fatalf("the link works once: %d", r.Status)
	}
	if r := e.post("/api/auth/login", map[string]any{"email": email, "password": password}, ""); r.Status != 200 {
		t.Fatalf("a confirmed address signs in: %d", r.Status)
	}
	if r := e.post("/api/auth/confirm-email", map[string]any{"userId": linkParam(t, link, "confirm"), "token": "garbage"}, ""); r.Status != 400 {
		t.Fatal("a garbage token is a 400")
	}
}

func TestResendConfirmationLooksTheSameForEveryone(t *testing.T) {
	e := newEnv(t, withConfirmedEmail())
	email := newEmail()
	e.post("/api/auth/register", map[string]any{"email": email, "password": password}, "")
	beforeMsgs, _ := e.app.Mailbox.For(context.Background(), email)
	before := len(beforeMsgs)
	known := e.post("/api/auth/resend-confirmation", map[string]any{"email": email}, "")
	unknown := e.post("/api/auth/resend-confirmation", map[string]any{"email": newEmail()}, "")
	if known.Status != 202 || unknown.Status != 202 || known.str("message") != unknown.str("message") {
		t.Fatalf("the answer must not reveal who has an account: %d / %d", known.Status, unknown.Status)
	}
	if afterMsgs, _ := e.app.Mailbox.For(context.Background(), email); len(afterMsgs) != before+1 {
		t.Fatal("a pending address gets a new link")
	}
}

func TestPasswordResetFlow(t *testing.T) {
	e := newEnv(t)
	u := e.register()
	if r := e.post("/api/auth/forgot-password", map[string]any{"email": newEmail()}, ""); r.Status != 202 {
		t.Fatalf("an unknown address looks the same: %d", r.Status)
	}
	if r := e.post("/api/auth/forgot-password", map[string]any{"email": u.Email}, ""); r.Status != 202 {
		t.Fatalf("forgot: %d", r.Status)
	}
	link := lastMail(t, e, u.Email).Link
	reset := func(pw string) reply {
		return e.post("/api/auth/reset-password", map[string]any{"userId": linkParam(t, link, "reset"), "token": linkParam(t, link, "token"), "newPassword": pw}, "")
	}
	if r := reset("weak"); r.Status != 400 {
		t.Fatalf("the policy still applies: %d", r.Status)
	}
	if r := reset("Another-Passw0rd9"); r.Status != 204 {
		t.Fatalf("reset: %d %s", r.Status, r.Raw)
	}
	if r := reset("Third-Passw0rd9"); r.Status != 400 {
		t.Fatalf("a reset link works once: %d", r.Status)
	}
	if e.get("/api/auth/me", u.Token).Status != 401 {
		t.Fatal("a reset ends every session")
	}
	if e.post("/api/auth/login", map[string]any{"email": u.Email, "password": password}, "").Status != 401 ||
		e.post("/api/auth/login", map[string]any{"email": u.Email, "password": "Another-Passw0rd9"}, "").Status != 200 {
		t.Fatal("the old password stops working, the new one signs in")
	}
	if r := e.post("/api/auth/reset-password", map[string]any{"userId": linkParam(t, link, "reset"), "token": "garbage", "newPassword": "Fourth-Passw0rd9"}, ""); r.Status != 400 {
		t.Fatal("a garbage token is a 400")
	}
}

func TestTwoFactorLifecycle(t *testing.T) {
	e := newEnv(t)
	u := e.register()
	if e.get("/api/auth/mfa", u.Token).Body["enabled"] != false {
		t.Fatal("off by default")
	}
	setup := e.post("/api/auth/mfa/setup", nil, u.Token)
	key := setup.str("sharedKey")
	if setup.Status != 200 || key == "" || !strings.HasPrefix(setup.str("otpAuthUri"), "otpauth://totp/") {
		t.Fatalf("setup: %d %s", setup.Status, setup.Raw)
	}
	if r := e.post("/api/auth/mfa/enable", map[string]any{"code": "000000"}, u.Token); r.Status != 400 {
		t.Fatalf("a wrong code does not turn it on: %d", r.Status)
	}
	good, _ := identity.TOTPCode(key, time.Now())
	r := e.post("/api/auth/mfa/enable", map[string]any{"code": good}, u.Token)
	codes, _ := r.Body["codes"].([]any)
	if r.Status != 200 || len(codes) != 8 {
		t.Fatalf("enable: %d %s", r.Status, r.Raw)
	}
	if e.post("/api/auth/mfa/setup", nil, u.Token).Status != 409 {
		t.Fatal("setup while on is a conflict")
	}

	login := func(code string) reply {
		b := map[string]any{"email": u.Email, "password": password}
		if code != "" {
			b["code"] = code
		}
		return e.post("/api/auth/login", b, "")
	}
	if r := login(""); r.Status != 401 || r.str("title") != identity.MfaRequired {
		t.Fatalf("a code is required: %d %s", r.Status, r.Raw)
	}
	if r := login("111111"); r.Status != 401 {
		t.Fatalf("a wrong code: %d", r.Status)
	}
	now, _ := identity.TOTPCode(key, time.Now())
	if r := login(now); r.Status != 200 {
		t.Fatalf("the authenticator code signs in: %d %s", r.Status, r.Raw)
	}
	recovery := codes[0].(string)
	if r := login(recovery); r.Status != 200 {
		t.Fatalf("a recovery code signs in once: %d %s", r.Status, r.Raw)
	}
	if r := login(recovery); r.Status != 401 {
		t.Fatalf("a recovery code works once: %d", r.Status)
	}

	session := sessionOf(t, login(now), u.Email)
	if r := e.post("/api/auth/mfa/disable", map[string]any{"password": "wrong", "code": now}, session.Token); r.Status != 400 {
		t.Fatalf("disable needs the password: %d", r.Status)
	}
	if r := e.post("/api/auth/mfa/disable", map[string]any{"password": password, "code": now}, session.Token); r.Status != 204 {
		t.Fatalf("disable: %d", r.Status)
	}
	if r := login(""); r.Status != 200 {
		t.Fatalf("with two-factor off a password is enough: %d", r.Status)
	}
}

func TestTheAuthRateLimitIsSharedAndOnlyForAuth(t *testing.T) {
	e := newEnv(t, withRateLimit(5))
	ip := fmt.Sprintf("10.%d.%d.%d", rand.Intn(250)+1, rand.Intn(250)+1, rand.Intn(250)+1) // a client address of our own
	hdr := map[string]string{"X-Forwarded-For": ip}
	statuses := []int{}
	for i := 0; i < 8; i++ {
		statuses = append(statuses, e.do(call{method: "POST", path: "/api/auth/login", body: map[string]any{"email": "a@b.co", "password": "x"}, headers: hdr}).Status)
	}
	limited := 0
	for _, s := range statuses {
		if s == 429 {
			limited++
		}
	}
	if limited != 3 {
		t.Fatalf("5 per minute: the last 3 of 8 are refused, got %v", statuses)
	}
	if r := e.do(call{method: "GET", path: "/api/health", headers: hdr}); r.Status != 200 {
		t.Fatal("the limit applies to /api/auth only")
	}
	if r := e.do(call{method: "POST", path: "/api/auth/login", body: map[string]any{"email": "a@b.co", "password": "x"}, headers: map[string]string{"X-Forwarded-For": ip + "9"}}); r.Status == 429 {
		t.Fatal("another client has its own count")
	}
}

func TestJWKSAndDevMailboxSwitches(t *testing.T) {
	e := newEnv(t, withDevMailbox(false))
	if e.get("/.well-known/jwks.json", "").Status != 404 {
		t.Fatal("with a shared secret there is no public key")
	}
	if e.get("/api/dev/mailbox?to=a@b.co", "").Status != 404 {
		t.Fatal("the dev mailbox is off unless asked for")
	}
}

func TestRS256PublishesTheKeySetAndTheTokenVerifiesAgainstIt(t *testing.T) {
	e := newEnv(t, withRSA(testRSAPEM(t)))
	u := e.register()
	jwks := e.get("/.well-known/jwks.json", "")
	keys, _ := jwks.Body["keys"].([]any)
	if jwks.Status != 200 || len(keys) != 1 || keys[0].(map[string]any)["kid"] != "it-1" || keys[0].(map[string]any)["alg"] != "RS256" {
		t.Fatalf("jwks: %d %s", jwks.Status, jwks.Raw)
	}
	if e.get("/api/auth/me", u.Token).Status != 200 {
		t.Fatal("an RS256 token must be accepted")
	}
	_ = context.Background
}
