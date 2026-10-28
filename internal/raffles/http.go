package raffles

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-sole/internal/platform/httpx"
)

const (
	maxPairsTotal = 5000
	maxPriceCents = 1_000_000
)

// Raffle is the public view of a raffle. SeedHash is public from the start; Seed only after the draw.
type Raffle struct {
	ID         uuid.UUID  `json:"id"`
	Code       string     `json:"code"`
	Name       string     `json:"name"`
	Colorway   string     `json:"colorway"`
	Brand      string     `json:"brand"`
	DrawAt     time.Time  `json:"drawAt"`
	PriceCents int64      `json:"priceCents"`
	PairsTotal int        `json:"pairsTotal"`
	PairsLeft  int        `json:"pairsLeft"`
	Status     string     `json:"status"`
	SeedHash   string     `json:"seedHash"`
	Seed       *string    `json:"seed"`
	DrawnAt    *time.Time `json:"drawnAt"`
	Entries    int        `json:"entries"`
}

// DrawEntry is one entry as the draw view shows it.
type DrawEntry struct {
	OrderID uuid.UUID `json:"orderId"`
	Pairs   int       `json:"pairs"`
	Outcome string    `json:"outcome"`
	Rank    *int      `json:"rank"`
	Key     *string   `json:"key"`
}

// DrawView is what it takes to check a draw.
type DrawView struct {
	Code            string      `json:"code"`
	Status          string      `json:"status"`
	SeedHash        string      `json:"seedHash"`
	Seed            *string     `json:"seed"`
	SeedMatchesHash bool        `json:"seedMatchesHash"`
	Entries         int         `json:"entries"`
	Winners         int         `json:"winners"`
	Mine            []DrawEntry `json:"mine"`
}

// Info is what other modules may ask the raffles module (Orders never reads this schema).
type Info struct {
	ID         uuid.UUID
	Code       string
	PriceCents int64
	DrawAt     time.Time
	Open       bool
}

// Catalog answers the one cross-module read: a raffle's existence and price.
type Catalog struct{ Pool *pgxpool.Pool }

// Find looks a raffle up by code. It returns nil when there is none.
func (c Catalog) Find(ctx context.Context, code string) (*Info, error) {
	var i Info
	var status string
	err := c.Pool.QueryRow(ctx, `SELECT id, code, price_cents, draw_at, status FROM raffles.raffles WHERE code = $1`, code).
		Scan(&i.ID, &i.Code, &i.PriceCents, &i.DrawAt, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	i.Open = status == StatusOpen
	return &i, err
}

// API is the raffles HTTP surface.
type API struct {
	Pool *pgxpool.Pool
	Auth httpx.Authenticator
	Log  *slog.Logger
}

// Routes registers the raffle endpoints; every one needs a signed-in user.
func (a *API) Routes(mux *http.ServeMux) {
	authed := func(h httpx.HandlerFunc) http.HandlerFunc {
		return httpx.Authenticated(a.Log, a.Auth, httpx.Wrap(a.Log, h))
	}
	mux.HandleFunc("GET /api/raffles", authed(a.list))
	mux.HandleFunc("GET /api/raffles/{code}", authed(a.get))
	mux.HandleFunc("GET /api/raffles/{code}/draw", authed(a.draw))
	mux.HandleFunc("POST /api/raffles", httpx.Authenticated(a.Log, a.Auth, httpx.Staff(httpx.Wrap(a.Log, a.create))))
}

const raffleSelect = `SELECT r.id, r.code, r.name, r.colorway, r.brand, r.draw_at, r.price_cents, r.pairs_total, r.pairs_left, r.status,
	r.seed_hash, CASE WHEN r.status = 'Drawn' THEN r.seed END, r.drawn_at,
	(SELECT count(*) FROM raffles.raffle_entries e WHERE e.raffle_id = r.id) FROM raffles.raffles r`

func scanRaffle(row pgx.Row) (Raffle, error) {
	var r Raffle
	err := row.Scan(&r.ID, &r.Code, &r.Name, &r.Colorway, &r.Brand, &r.DrawAt, &r.PriceCents, &r.PairsTotal, &r.PairsLeft, &r.Status, &r.SeedHash, &r.Seed, &r.DrawnAt, &r.Entries)
	return r, err
}

func (a *API) list(w http.ResponseWriter, r *http.Request) error {
	rows, err := a.Pool.Query(r.Context(), raffleSelect+` ORDER BY r.draw_at, r.code`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []Raffle{}
	for rows.Next() {
		item, err := scanRaffle(rows)
		if err != nil {
			return err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, out)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) error {
	item, err := scanRaffle(a.Pool.QueryRow(r.Context(), raffleSelect+` WHERE r.code = $1`, r.PathValue("code")))
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.Fail(http.StatusNotFound, "Not found.")
	}
	if err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, item)
}

// draw returns what a person needs to check the draw: the seed (once drawn) and its hash, and their own entries with the
// key they were ordered by. Staff see every entry. Recompute: key = SHA-256("seed:orderId"), sort ascending, hand out
// pairs first fit.
func (a *API) draw(w http.ResponseWriter, r *http.Request) error {
	me := httpx.PrincipalOf(r.Context())
	var id uuid.UUID
	var code, status, seed, seedHash string
	err := a.Pool.QueryRow(r.Context(), `SELECT id, code, status, seed, seed_hash FROM raffles.raffles WHERE code = $1`, r.PathValue("code")).
		Scan(&id, &code, &status, &seed, &seedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.Fail(http.StatusNotFound, "Not found.")
	}
	if err != nil {
		return err
	}
	drawn := status == StatusDrawn
	rows, err := a.Pool.Query(r.Context(), `SELECT order_id, owner_id, pairs, outcome, rank FROM raffles.raffle_entries WHERE raffle_id = $1`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	view := DrawView{Code: code, Status: status, SeedHash: seedHash, Mine: []DrawEntry{}}
	if drawn {
		s := seed
		view.Seed = &s
		view.SeedMatchesHash = Matches(seed, seedHash)
	}
	for rows.Next() {
		var e DrawEntry
		var owner uuid.UUID
		if err := rows.Scan(&e.OrderID, &owner, &e.Pairs, &e.Outcome, &e.Rank); err != nil {
			return err
		}
		view.Entries++
		if e.Outcome == OutcomeWon {
			view.Winners++
		}
		if me.IsStaff() || owner == me.ID {
			if drawn {
				k := Key(seed, e.OrderID)
				e.Key = &k
			}
			view.Mine = append(view.Mine, e)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sort.SliceStable(view.Mine, func(i, j int) bool {
		a, b := view.Mine[i].Rank, view.Mine[j].Rank
		if a == nil || b == nil {
			return a != nil
		}
		return *a < *b
	})
	return httpx.JSON(w, http.StatusOK, view)
}

type createRequest struct {
	Code       string     `json:"code"`
	Name       string     `json:"name"`
	Colorway   string     `json:"colorway"`
	Brand      string     `json:"brand"`
	DrawAt     *time.Time `json:"drawAt"`
	PairsTotal int64      `json:"pairsTotal"`
	PriceCents int64      `json:"priceCents"`
}

// create opens a raffle. Staff only. The seed is generated here, and only its hash is shown until the draw.
func (a *API) create(w http.ResponseWriter, r *http.Request) error {
	var req createRequest
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	code := strings.TrimSpace(req.Code)
	if code == "" || len(code) > 20 {
		return httpx.Fail(http.StatusBadRequest, "Code is required and at most 20 characters.")
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Colorway) == "" || strings.TrimSpace(req.Brand) == "" {
		return httpx.Fail(http.StatusBadRequest, "Name, colorway and brand are required.")
	}
	if req.PairsTotal < 1 || req.PairsTotal > maxPairsTotal {
		return httpx.Fail(http.StatusBadRequest, "Pairs on sale must be between 1 and %d pairs.", maxPairsTotal)
	}
	if req.PriceCents < 1 || req.PriceCents > maxPriceCents {
		return httpx.Fail(http.StatusBadRequest, "Price must be between 1 and %d cents per pair.", maxPriceCents)
	}
	now := time.Now()
	drawAt := now.AddDate(0, 0, 7)
	if req.DrawAt != nil {
		drawAt = *req.DrawAt
	}
	if !drawAt.After(now) {
		return httpx.Fail(http.StatusBadRequest, "The draw time must be in the future.")
	}
	seed := NewSeed()
	id, _ := uuid.NewV7()
	_, err := a.Pool.Exec(r.Context(), `
		INSERT INTO raffles.raffles (id, code, name, colorway, brand, draw_at, price_cents, pairs_total, pairs_left, status, seed, seed_hash, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8,'Open',$9,$10,now())`,
		id, code, strings.TrimSpace(req.Name), strings.TrimSpace(req.Colorway), strings.TrimSpace(req.Brand), drawAt, req.PriceCents, req.PairsTotal, seed, Commitment(seed))
	if err != nil {
		if isUnique(err) {
			return httpx.Fail(http.StatusConflict, "Raffle '%s' already exists.", code)
		}
		return err
	}
	item, err := scanRaffle(a.Pool.QueryRow(r.Context(), raffleSelect+` WHERE r.id = $1`, id))
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/raffles/"+code)
	return httpx.JSON(w, http.StatusCreated, item)
}

func isUnique(err error) bool {
	var coder interface{ SQLState() string }
	return errors.As(err, &coder) && coder.SQLState() == "23505"
}
