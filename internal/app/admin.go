package app

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/joshbarros/golang-sole/internal/platform/httpx"
	"github.com/joshbarros/golang-sole/internal/platform/messaging"
)

// registerAdmin wires the dead-letter endpoints: staff can look inside the dead-letter queues and replay them.
func registerAdmin(mux *http.ServeMux, log *slog.Logger, auth httpx.Authenticator, dl *messaging.DeadLetters) {
	need := func() (*messaging.DeadLetters, error) {
		if dl == nil {
			return nil, httpx.Fail(http.StatusServiceUnavailable, "Messaging is switched off in this process.")
		}
		return dl, nil
	}
	mux.HandleFunc("GET /api/admin/dead-letters", httpx.Authenticated(log, auth, httpx.Staff(httpx.Wrap(log, func(w http.ResponseWriter, r *http.Request) error {
		d, err := need()
		if err != nil {
			return err
		}
		peek, _ := strconv.Atoi(r.URL.Query().Get("peek"))
		if r.URL.Query().Get("peek") == "" {
			peek = 10
		}
		out, err := d.List(r.Context(), min(max(peek, 0), 50))
		if err != nil {
			return err
		}
		return httpx.JSON(w, http.StatusOK, out)
	}))))
	mux.HandleFunc("POST /api/admin/dead-letters/{queue}/replay", httpx.Authenticated(log, auth, httpx.Staff(httpx.Wrap(log, func(w http.ResponseWriter, r *http.Request) error {
		d, err := need()
		if err != nil {
			return err
		}
		maxN := 100
		if v, err := strconv.Atoi(r.URL.Query().Get("max")); err == nil {
			maxN = min(max(v, 1), 1000)
		}
		res, err := d.Replay(r.Context(), r.PathValue("queue"), maxN)
		if err != nil {
			return err
		}
		return httpx.JSON(w, http.StatusOK, res)
	}))))
}
