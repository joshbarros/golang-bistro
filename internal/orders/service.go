package orders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-bistro/internal/platform/httpx"
	"github.com/joshbarros/golang-bistro/internal/platform/messaging"
	"github.com/joshbarros/golang-bistro/internal/platform/pgdb"
	"github.com/joshbarros/golang-bistro/internal/sittings"
)

// Schema and consumer names.
const (
	Schema       = "orders"
	ConsumerName = "orders"
)

// Order statuses.
const (
	Requested  = "Requested"
	Waitlisted = "Waitlisted"
	Held       = "Held"
	Sold       = "Sold"
	Refunded   = "Refunded"
	Rejected   = "Rejected"
)

// Order is the public view of an order.
type Order struct {
	ID                 uuid.UUID  `json:"id"`
	OwnerID            uuid.UUID  `json:"ownerId"`
	SittingCode        string     `json:"sittingCode"`
	Reference          string     `json:"reference"`
	Covers             int        `json:"covers"`
	TotalCents         int64      `json:"totalCents"`
	Status             string     `json:"status"`
	Reason             *string    `json:"reason"`
	ConfirmationNumber *string    `json:"confirmationNumber"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
	CoversReleasedAt   *time.Time `json:"coversReleasedAt"`
}

// SittingCatalog is the one thing Orders may ask the sittings module.
type SittingCatalog interface {
	Find(ctx context.Context, code string) (*sittings.Info, error)
}

// Service is the HTTP side of the saga: it writes the order and its first outbox row in one local transaction.
type Service struct {
	Pool     *pgxpool.Pool
	Outbox   messaging.Outbox
	Sittings SittingCatalog
	Log      *slog.Logger

	// FinalMarkDelay is a latency knob for the final mark: Kubernetes sets it so the refund race window is wide enough to hit on purpose.
	FinalMarkDelay time.Duration
}

// NewService builds a Service with its outbox.
func NewService(pool *pgxpool.Pool, catalog SittingCatalog, log *slog.Logger) *Service {
	return &Service{Pool: pool, Outbox: messaging.Outbox{Schema: Schema, Module: "orders"}, Sittings: catalog, Log: log}
}

const orderColumns = `id, owner_id, sitting_code, reference, covers, total_cents, status, reason, confirmation_number, created_at, updated_at, covers_released_at`

func scanOrder(row pgx.Row) (Order, error) {
	var o Order
	err := row.Scan(&o.ID, &o.OwnerID, &o.SittingCode, &o.Reference, &o.Covers, &o.TotalCents, &o.Status, &o.Reason, &o.ConfirmationNumber, &o.CreatedAt, &o.UpdatedAt, &o.CoversReleasedAt)
	return o, err
}

// CreateRequest is the body of POST /api/orders.
type CreateRequest struct {
	SittingCode string `json:"sittingCode"`
	Reference   string `json:"reference"`
	Covers      int64  `json:"covers"`
	Waitlist    bool   `json:"waitlist"`
}

// Create reserves covers in the sitting for the caller: validates, writes the order and its reserve command in one transaction.
func (s *Service) Create(ctx context.Context, caller *httpx.Principal, req CreateRequest) (Order, error) {
	if caller == nil {
		return Order{}, httpx.Fail(http.StatusUnauthorized, "Sign in to book.")
	}
	if strings.TrimSpace(req.SittingCode) == "" {
		return Order{}, httpx.Fail(http.StatusBadRequest, "Sitting code is required.")
	}
	if msg := Validate(req.Reference, req.Covers); msg != "" {
		return Order{}, httpx.Fail(http.StatusBadRequest, "%s", msg)
	}
	sitting, err := s.Sittings.Find(ctx, strings.TrimSpace(req.SittingCode))
	if err != nil {
		return Order{}, err
	}
	if sitting == nil {
		return Order{}, httpx.Fail(http.StatusNotFound, "Sitting '%s' does not exist.", req.SittingCode)
	}
	now := time.Now().UTC()
	covers := int(req.Covers)
	reference := strings.TrimSpace(req.Reference)
	total, err := TotalCents(sitting.PriceCents, covers)
	if err != nil {
		return Order{}, httpx.Fail(http.StatusBadRequest, "The total is too large.")
	}
	id, _ := uuid.NewV7()
	o := Order{ID: id, OwnerID: caller.ID, SittingCode: sitting.Code, Reference: reference, Covers: covers, TotalCents: total, Status: Requested, CreatedAt: now, UpdatedAt: now}
	err = pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO orders.orders (id, owner_id, sitting_id, sitting_code, reference, covers, total_cents, status, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'Requested',$8,$8)`, o.ID, o.OwnerID, sitting.ID, o.SittingCode, o.Reference, o.Covers, o.TotalCents, now); err != nil {
			return err
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ReserveKey, o.ID,
			messaging.ReserveCovers{OrderID: o.ID, OwnerID: caller.ID, SittingID: sitting.ID, Covers: covers, Reference: reference, Waitlist: req.Waitlist})
	})
	if err != nil {
		if pgdb.IsUniqueViolation(err) {
			return Order{}, httpx.Fail(http.StatusConflict, "Reference '%s' is already booked on %s.", reference, sitting.Code)
		}
		return Order{}, err
	}
	return o, nil
}

// Get returns an order to its owner or to staff. Anyone else gets a 404, so ids cannot be probed.
func (s *Service) Get(ctx context.Context, caller *httpx.Principal, id uuid.UUID) (Order, error) {
	o, err := scanOrder(s.Pool.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders.orders WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !caller.CanAccess(o.OwnerID)) {
		return Order{}, httpx.Fail(http.StatusNotFound, "Order not found.")
	}
	return o, err
}

// List returns the caller's orders (staff: everyone's), newest first, a page at a time, with the total.
func (s *Service) List(ctx context.Context, caller *httpx.Principal, sittingCode string, page, pageSize int) ([]Order, int, error) {
	if caller == nil {
		return nil, 0, httpx.Fail(http.StatusUnauthorized, "Sign in to see orders.")
	}
	where := []string{"true"}
	args := []any{}
	if !caller.IsStaff() {
		args = append(args, caller.ID)
		where = append(where, "owner_id = $"+strconv.Itoa(len(args)))
	}
	if strings.TrimSpace(sittingCode) != "" {
		args = append(args, sittingCode)
		where = append(where, "sitting_code = $"+strconv.Itoa(len(args)))
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM orders.orders WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := s.Pool.Query(ctx, fmt.Sprintf(`SELECT %s FROM orders.orders WHERE %s ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`,
		orderColumns, cond, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, o)
	}
	return items, total, rows.Err()
}

// Refund moves Waitlisted, Held or Sold to Refunded and asks Sittings to give the covers back (or to drop the place in the line). The confirmation is not touched.
func (s *Service) Refund(ctx context.Context, caller *httpx.Principal, id uuid.UUID) (Order, error) {
	if _, err := s.Get(ctx, caller, id); err != nil { // 404 unless the caller may see it
		return Order{}, err
	}
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET status = 'Refunded', reason = 'Refunded by request.', updated_at = now()
			WHERE id = $1 AND status IN ('Held','Sold','Waitlisted')`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			var current string
			if err := tx.QueryRow(ctx, `SELECT status FROM orders.orders WHERE id = $1`, id).Scan(&current); err != nil {
				return err
			}
			return httpx.Fail(http.StatusConflict, "An order in status %s cannot be refunded.", current)
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ReleaseKey, id, messaging.ReleaseCovers{OrderID: id, Reason: "refund"})
	})
	if err != nil {
		return Order{}, err
	}
	return scanOrder(s.Pool.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders.orders WHERE id = $1`, id))
}
