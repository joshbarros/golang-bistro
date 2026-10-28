package sittings

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-bistro/internal/platform/httpx"
)

const (
	maxCoversTotal  = 5000
	maxPriceCents   = 1_000_000
	defaultPerGuest = 8
)

// Sitting is the public view of a sitting.
type Sitting struct {
	ID          uuid.UUID `json:"id"`
	Code        string    `json:"code"`
	Name        string    `json:"name"`
	Room        string    `json:"room"`
	Chef        string    `json:"chef"`
	ServiceDate time.Time `json:"serviceDate"`
	PriceCents  int64     `json:"priceCents"`
	CoversTotal int       `json:"coversTotal"`
	CoversLeft  int       `json:"coversLeft"`
	MaxPerGuest int       `json:"maxPerGuest"`
}

// Info is what other modules may ask the sittings module (Orders never reads this schema).
type Info struct {
	ID         uuid.UUID
	Code       string
	PriceCents int64
}

// Catalog answers the one cross-module read: a sitting's existence and price.
type Catalog struct{ Pool *pgxpool.Pool }

// Find looks a sitting up by code. It returns nil when there is none.
func (c Catalog) Find(ctx context.Context, code string) (*Info, error) {
	var i Info
	err := c.Pool.QueryRow(ctx, `SELECT id, code, price_cents FROM sittings.sittings WHERE code = $1`, code).Scan(&i.ID, &i.Code, &i.PriceCents)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &i, err
}

// API is the sittings HTTP surface.
type API struct {
	Pool *pgxpool.Pool
	Auth httpx.Authenticator
	Log  *slog.Logger
}

// Routes registers the sitting endpoints; every one needs a signed-in user.
func (a *API) Routes(mux *http.ServeMux) {
	authed := func(h httpx.HandlerFunc) http.HandlerFunc {
		return httpx.Authenticated(a.Log, a.Auth, httpx.Wrap(a.Log, h))
	}
	mux.HandleFunc("GET /api/sittings", authed(a.list))
	mux.HandleFunc("GET /api/sittings/{code}", authed(a.get))
	mux.HandleFunc("POST /api/sittings", httpx.Authenticated(a.Log, a.Auth, httpx.Staff(httpx.Wrap(a.Log, a.create))))
}

const sittingSelect = `SELECT id, code, name, room, chef, service_date, price_cents, covers_total, covers_left, max_per_guest FROM sittings.sittings`

func scanSitting(row pgx.Row) (Sitting, error) {
	var d Sitting
	err := row.Scan(&d.ID, &d.Code, &d.Name, &d.Room, &d.Chef, &d.ServiceDate, &d.PriceCents, &d.CoversTotal, &d.CoversLeft, &d.MaxPerGuest)
	return d, err
}

func (a *API) list(w http.ResponseWriter, r *http.Request) error {
	rows, err := a.Pool.Query(r.Context(), sittingSelect+` ORDER BY service_date, code`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []Sitting{}
	for rows.Next() {
		d, err := scanSitting(rows)
		if err != nil {
			return err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, out)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) error {
	d, err := scanSitting(a.Pool.QueryRow(r.Context(), sittingSelect+` WHERE code = $1`, r.PathValue("code")))
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.Fail(http.StatusNotFound, "Not found.")
	}
	if err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, d)
}

type createRequest struct {
	Code        string     `json:"code"`
	Name        string     `json:"name"`
	Room        string     `json:"room"`
	Chef        string     `json:"chef"`
	ServiceDate *time.Time `json:"serviceDate"`
	CoversTotal int64      `json:"coversTotal"`
	PriceCents  int64      `json:"priceCents"`
	MaxPerGuest *int64     `json:"maxPerGuest"`
}

// create opens a sitting. Staff only. This is the only place covers_left is initialised; after that only the reserve and
// release handlers write it.
func (a *API) create(w http.ResponseWriter, r *http.Request) error {
	var req createRequest
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	code := strings.TrimSpace(req.Code)
	if code == "" || len(code) > 20 {
		return httpx.Fail(http.StatusBadRequest, "Code is required and at most 20 characters.")
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Room) == "" || strings.TrimSpace(req.Chef) == "" {
		return httpx.Fail(http.StatusBadRequest, "Name, room and chef are required.")
	}
	if req.CoversTotal < 1 || req.CoversTotal > maxCoversTotal {
		return httpx.Fail(http.StatusBadRequest, "Covers on sale must be between 1 and %d covers.", maxCoversTotal)
	}
	if req.PriceCents < 1 || req.PriceCents > maxPriceCents {
		return httpx.Fail(http.StatusBadRequest, "Price must be between 1 and %d cents per cover.", maxPriceCents)
	}
	limit := min(int64(defaultPerGuest), req.CoversTotal)
	if req.MaxPerGuest != nil {
		limit = *req.MaxPerGuest
	}
	if limit < 1 || limit > req.CoversTotal {
		return httpx.Fail(http.StatusBadRequest, "Per-guest cap must be at least 1 and no more than the covers on sale.")
	}
	release := time.Now().AddDate(0, 0, 14)
	if req.ServiceDate != nil {
		release = *req.ServiceDate
	}
	id, _ := uuid.NewV7()
	_, err := a.Pool.Exec(r.Context(), `
		INSERT INTO sittings.sittings (id, code, name, room, chef, service_date, price_cents, covers_total, covers_left, max_per_guest, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8,$9,now())`,
		id, code, strings.TrimSpace(req.Name), strings.TrimSpace(req.Room), strings.TrimSpace(req.Chef), release, req.PriceCents, req.CoversTotal, limit)
	if err != nil {
		var coder interface{ SQLState() string }
		if errors.As(err, &coder) && coder.SQLState() == "23505" {
			return httpx.Fail(http.StatusConflict, "Sitting '%s' already exists.", code)
		}
		return err
	}
	d, err := scanSitting(a.Pool.QueryRow(r.Context(), sittingSelect+` WHERE id = $1`, id))
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/sittings/"+code)
	return httpx.JSON(w, http.StatusCreated, d)
}
