package orders

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-sole/internal/platform/httpx"
	"github.com/joshbarros/golang-sole/internal/platform/messaging"
)

// API is the orders HTTP surface.
type API struct {
	Svc   *Service
	Kafka *messaging.Kafka // nil when messaging is off
	Auth  httpx.Authenticator
	Log   *slog.Logger
}

// Routes registers the order endpoints; every one needs a signed-in user.
func (a *API) Routes(mux *http.ServeMux) {
	authed := func(h httpx.HandlerFunc) http.HandlerFunc {
		return httpx.Authenticated(a.Log, a.Auth, httpx.Wrap(a.Log, h))
	}
	mux.HandleFunc("POST /api/orders", authed(a.create))
	mux.HandleFunc("GET /api/orders", authed(a.list))
	mux.HandleFunc("GET /api/orders/{id}", authed(a.get))
	mux.HandleFunc("POST /api/orders/{id}/refund", authed(a.refund))
	mux.HandleFunc("GET /api/orders/{id}/events", httpx.Authenticated(a.Log, a.Auth, httpx.Staff(httpx.Wrap(a.Log, a.events))))
}

func orderID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return uuid.Nil, httpx.Fail(http.StatusNotFound, "Order not found.")
	}
	return id, nil
}

// create accepts the order for the signed-in user. Raffles and the claim follow asynchronously; poll GET to see them.
func (a *API) create(w http.ResponseWriter, r *http.Request) error {
	var req CreateRequest
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	o, err := a.Svc.Create(r.Context(), httpx.PrincipalOf(r.Context()), req)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/orders/"+o.ID.String())
	return httpx.JSON(w, http.StatusAccepted, o)
}

// list returns your orders, newest first, a page at a time (default 100, at most 100). Staff see everyone's. The total is in X-Total-Count.
func (a *API) list(w http.ResponseWriter, r *http.Request) error {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	size := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("pageSize")); err == nil {
		size = min(max(v, 1), 100)
	}
	items, total, err := a.Svc.List(r.Context(), httpx.PrincipalOf(r.Context()), r.URL.Query().Get("raffleCode"), page, size)
	if err != nil {
		return err
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	return httpx.JSON(w, http.StatusOK, items)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) error {
	id, err := orderID(r)
	if err != nil {
		return err
	}
	o, err := a.Svc.Get(r.Context(), httpx.PrincipalOf(r.Context()), id)
	if err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, o)
}

func (a *API) refund(w http.ResponseWriter, r *http.Request) error {
	id, err := orderID(r)
	if err != nil {
		return err
	}
	o, err := a.Svc.Refund(r.Context(), httpx.PrincipalOf(r.Context()), id)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/orders/"+id.String())
	return httpx.JSON(w, http.StatusAccepted, o)
}

// events is the Kafka event log for one order, read from the start of the topic. Staff only.
func (a *API) events(w http.ResponseWriter, r *http.Request) error {
	id, err := orderID(r)
	if err != nil {
		return err
	}
	if a.Kafka == nil {
		return httpx.Fail(http.StatusServiceUnavailable, "Messaging is switched off in this process.")
	}
	values, err := a.Kafka.Read(r.Context(), id.String(), 10*time.Second)
	if err != nil {
		return err
	}
	out := make([]json.RawMessage, 0, len(values))
	for _, v := range values {
		out = append(out, json.RawMessage(v))
	}
	return httpx.JSON(w, http.StatusOK, out)
}
