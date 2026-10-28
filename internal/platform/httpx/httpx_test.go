package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

var quiet = slog.New(slog.DiscardHandler)

func TestProblemDetailsCarryTheCorrelationId(t *testing.T) {
	h := Correlation(Wrap(quiet, func(w http.ResponseWriter, r *http.Request) error {
		return Fail(http.StatusConflict, "already %s", "there")
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(CorrelationHeader, "abc-123")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Title != "already there" || p.Status != 409 || p.CorrelationID != "abc-123" || rec.Header().Get(CorrelationHeader) != "abc-123" {
		t.Fatalf("unexpected problem %+v", p)
	}
}

func TestUnexpectedErrorsAreHiddenFromTheCaller(t *testing.T) {
	h := Correlation(Wrap(quiet, func(http.ResponseWriter, *http.Request) error {
		return errors.New("pq: password authentication failed for user sole")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("a 500 must say nothing about the cause: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPanicBecomesAProblem(t *testing.T) {
	h := Correlation(Recover(quiet, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "correlationId") {
		t.Fatalf("a panic must be a 500 problem: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCorrelationIdsAreAcceptedOnlyWhenSafe(t *testing.T) {
	for id, ok := range map[string]bool{"abc-123_X": true, "": false, "has space": false, "new\nline": false, strings.Repeat("a", 65): false, strings.Repeat("a", 64): true, "ünï": false} {
		if AcceptableCorrelationID(id) != ok {
			t.Errorf("AcceptableCorrelationID(%q) should be %v", id, ok)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(CorrelationHeader, "bad id\r\nSet-Cookie: x=1")
	Correlation(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req)
	if got := rec.Header().Get(CorrelationHeader); got == "" || strings.ContainsAny(got, " \r\n") {
		t.Fatalf("an unsafe id must be replaced, got %q", got)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := SecurityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/orders", nil))
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'"} {
		if rec.Header().Get(k) != v {
			t.Errorf("%s = %q, want %q", k, rec.Header().Get(k), v)
		}
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	if rec.Header().Get("Content-Security-Policy") != "" {
		t.Error("the strict CSP is for the API only")
	}
}

type fakeAuth struct{ err error }

func (f fakeAuth) Authenticate(_ context.Context, bearer string) (*Principal, error) {
	if f.err != nil {
		return nil, f.err
	}
	if bearer == "staff" {
		return &Principal{ID: uuid.New(), Roles: []string{RoleStaff}}, nil
	}
	return &Principal{ID: uuid.New(), Roles: []string{RoleCustomer}}, nil
}

func TestAuthenticatedAndStaffGuards(t *testing.T) {
	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }
	call := func(h http.HandlerFunc, header string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}
	authed := Authenticated(quiet, fakeAuth{}, ok)
	if call(authed, "") != 401 || call(authed, "Basic abc") != 401 || call(authed, "Bearer ") != 401 || call(authed, "Bearer x") != 204 {
		t.Error("only a bearer token gets through")
	}
	if call(Authenticated(quiet, fakeAuth{err: ErrInvalidToken}, ok), "Bearer x") != 401 {
		t.Error("a refused token is a 401")
	}
	staffOnly := Authenticated(quiet, fakeAuth{}, Staff(ok))
	if call(staffOnly, "Bearer customer") != 403 || call(staffOnly, "Bearer staff") != 204 {
		t.Error("staff routes: customers get 403, staff get through")
	}
}

func TestOwnerOrStaffAccess(t *testing.T) {
	owner := uuid.New()
	var nobody *Principal
	cases := []struct {
		p    *Principal
		want bool
	}{
		{&Principal{ID: owner, Roles: []string{RoleCustomer}}, true},
		{&Principal{ID: uuid.New(), Roles: []string{RoleCustomer}}, false},
		{&Principal{ID: uuid.New(), Roles: []string{RoleStaff}}, true},
		{&Principal{ID: uuid.New(), Roles: []string{RoleAdmin}}, true},
		{nobody, false},
	}
	for i, c := range cases {
		if c.p.CanAccess(owner) != c.want {
			t.Errorf("case %d: CanAccess should be %v", i, c.want)
		}
	}
}

func TestDecode(t *testing.T) {
	var v struct {
		Name string `json:"name"`
	}
	if err := Decode(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"NAME":"x"}`)), &v); err != nil || v.Name != "x" {
		t.Fatalf("field names are case-insensitive: %v %+v", err, v)
	}
	var de *DomainError
	if err := Decode(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{broken`)), &v); !errors.As(err, &de) || de.Status != 400 {
		t.Fatalf("malformed JSON is a 400: %v", err)
	}
	if err := Decode(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(``)), &v); err != nil {
		t.Fatalf("an empty body is the zero request: %v", err)
	}
}
