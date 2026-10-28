package sittings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-bistro/internal/platform/messaging"
	"github.com/joshbarros/golang-bistro/internal/platform/pgdb"
)

// Schema and consumer names.
const (
	Schema       = "sittings"
	ConsumerName = "sittings"

	HoldHeld     = "Held"
	HoldReleased = "Released"
)

// ReserveOutcome is what a reserve did.
type ReserveOutcome int

// The ways a reserve can end.
const (
	Held ReserveOutcome = iota
	Short
	CapReached
	Waitlisted
	Duplicate
)

// Service is the sittings module's logic. It is the only writer of covers_left and of the per-guest counts.
type Service struct {
	Pool   *pgxpool.Pool
	Outbox messaging.Outbox
	Log    *slog.Logger
}

// NewService builds a Service with its outbox.
func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{Pool: pool, Outbox: messaging.Outbox{Schema: Schema, Module: "sittings"}, Log: log}
}

// Handle implements messaging.Handler for the sittings queue.
func (s *Service) Handle(ctx context.Context, d messaging.Delivery) error {
	switch d.RoutingKey {
	case messaging.ReserveKey:
		cmd, err := messaging.Parse[messaging.ReserveCovers](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Reserve(ctx, d.MessageID, cmd)
		return err
	case messaging.ReleaseKey:
		cmd, err := messaging.Parse[messaging.ReleaseCovers](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Release(ctx, d.MessageID, cmd)
		return err
	}
	return fmt.Errorf("sittings does not handle %q", d.RoutingKey)
}

var errRefused = errors.New("the cap or the stock said no")

// Reserve takes covers from a sitting in two atomic steps inside one transaction: a per-guest cap upsert that only moves
// while the new total still fits, then one conditional UPDATE that matches only while the remaining stock covers the
// request. If either says no, the whole attempt rolls back (so a cap increment never outlives a failed reserve) and the
// refusal is recorded in a fresh transaction.
func (s *Service) Reserve(ctx context.Context, messageID uuid.UUID, cmd messaging.ReserveCovers) (ReserveOutcome, error) {
	outcome := Held
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		// 1. the per-guest cap: one atomic upsert that only moves while the new total still fits
		capTag, err := tx.Exec(ctx, `
			INSERT INTO sittings.guests (sitting_id, owner_id, covers)
			SELECT d.id, $2, $3 FROM sittings.sittings d WHERE d.id = $1 AND d.max_per_guest >= $3
			ON CONFLICT (sitting_id, owner_id) DO UPDATE SET covers = sittings.guests.covers + $3
			WHERE sittings.guests.covers + $3 <= (SELECT max_per_guest FROM sittings.sittings WHERE id = $1)`,
			cmd.SittingID, cmd.OwnerID, cmd.Covers)
		if err != nil {
			return err
		}
		if capTag.RowsAffected() == 1 {
			// 2. the remaining covers: one conditional UPDATE, the only writer of covers_left
			stockTag, err := tx.Exec(ctx, `UPDATE sittings.sittings SET covers_left = covers_left - $2 WHERE id = $1 AND covers_left >= $2
				AND NOT EXISTS (SELECT 1 FROM sittings.waitlist w WHERE w.sitting_id = $1)`, cmd.SittingID, cmd.Covers)
			if err != nil {
				return err
			}
			if stockTag.RowsAffected() == 1 {
				if _, err := tx.Exec(ctx, `INSERT INTO sittings.cover_holds (order_id, owner_id, sitting_id, covers, status, created_at) VALUES ($1,$2,$3,$4,'Held',now())`,
					cmd.OrderID, cmd.OwnerID, cmd.SittingID, cmd.Covers); err != nil {
					return err
				}
				return s.Outbox.Enqueue(ctx, tx, messaging.HeldKey, cmd.OrderID,
					messaging.CoversHeld{OrderID: cmd.OrderID, OwnerID: cmd.OwnerID, SittingID: cmd.SittingID, Covers: cmd.Covers, Reference: cmd.Reference})
			}
		}
		return errRefused // roll the cap increment back with everything else
	})
	if err == nil {
		return outcome, nil
	}
	if !errors.Is(err, errRefused) {
		return 0, err
	}
	// The refusal is recorded in a fresh transaction (the inbox claim was rolled back with the attempt).
	err = pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		var left, limit, total int
		_ = tx.QueryRow(ctx, `SELECT covers_left, max_per_guest, covers_total FROM sittings.sittings WHERE id = $1`, cmd.SittingID).Scan(&left, &limit, &total)
		var holding int
		_ = tx.QueryRow(ctx, `SELECT covers FROM sittings.guests WHERE sitting_id = $1 AND owner_id = $2`, cmd.SittingID, cmd.OwnerID).Scan(&holding)
		reason := "stock"
		outcome = Short
		if limit > 0 && holding+cmd.Covers > limit {
			reason = "cap"
			outcome = CapReached
		}
		if cmd.Waitlist && total > 0 && reason == "stock" && cmd.Covers <= total {
			// the sitting is full (or a line exists): the guest asked to wait, so the order takes a place in the line
			var seq int64
			if err := tx.QueryRow(ctx, `INSERT INTO sittings.waitlist (order_id, owner_id, sitting_id, covers, reference, created_at) VALUES ($1,$2,$3,$4,$5,now()) RETURNING seq`,
				cmd.OrderID, cmd.OwnerID, cmd.SittingID, cmd.Covers, cmd.Reference).Scan(&seq); err != nil {
				return err
			}
			var position int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM sittings.waitlist WHERE sitting_id = $1 AND seq <= $2`, cmd.SittingID, seq).Scan(&position); err != nil {
				return err
			}
			outcome = Waitlisted
			return s.Outbox.Enqueue(ctx, tx, messaging.WaitlistedKey, cmd.OrderID,
				messaging.CoversWaitlisted{OrderID: cmd.OrderID, Reference: cmd.Reference, Covers: cmd.Covers, Position: position})
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ShortKey, cmd.OrderID,
			messaging.CoversShort{OrderID: cmd.OrderID, Reference: cmd.Reference, RequestedCovers: cmd.Covers, CoversLeft: left, Reason: reason, PerGuestLimit: limit})
	})
	return outcome, err
}

// Release gives held covers back exactly once, returning the guest's allowance too. A second release changes nothing.
func (s *Service) Release(ctx context.Context, messageID uuid.UUID, cmd messaging.ReleaseCovers) (bool, error) {
	released := false
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil || !first {
			return err
		}
		// An order still in the line has no hold: leaving the line is a conditional DELETE. If a promotion won the row first,
		// this deletes nothing and the hold the promotion created is released below.
		left, err := tx.Exec(ctx, `DELETE FROM sittings.waitlist WHERE order_id = $1`, cmd.OrderID)
		if err != nil {
			return err
		}
		if left.RowsAffected() == 1 {
			released = true
			return s.Outbox.Enqueue(ctx, tx, messaging.ReleasedKey, cmd.OrderID, messaging.CoversReleased{OrderID: cmd.OrderID, Covers: 0, Reason: cmd.Reason})
		}
		var sittingID, ownerID uuid.UUID
		var covers int
		err = tx.QueryRow(ctx, `
			UPDATE sittings.cover_holds SET status = 'Released', released_at = now(), release_reason = $2
			WHERE order_id = $1 AND status = 'Held' RETURNING sitting_id, owner_id, covers`, cmd.OrderID, cmd.Reason).Scan(&sittingID, &ownerID, &covers)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // nothing held, or already released
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sittings.sittings SET covers_left = covers_left + $2 WHERE id = $1 AND covers_left + $2 <= covers_total`, sittingID, covers); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sittings.guests SET covers = covers - $3 WHERE sitting_id = $1 AND owner_id = $2 AND covers >= $3`, sittingID, ownerID, covers); err != nil {
			return err
		}
		released = true
		if err := s.Outbox.Enqueue(ctx, tx, messaging.ReleasedKey, cmd.OrderID, messaging.CoversReleased{OrderID: cmd.OrderID, Covers: covers, Reason: cmd.Reason}); err != nil {
			return err
		}
		return s.promote(ctx, tx, sittingID)
	})
	return released, err
}

// promote serves the waitlist first come, first served with the covers that came back. Strict order: if the head of the
// line does not fit yet, nobody behind it jumps ahead. It runs inside the release transaction, which already holds the
// sitting row, so two promotions for one sitting never run at once; the DELETE is the claim on each entry.
func (s *Service) promote(ctx context.Context, tx pgx.Tx, sittingID uuid.UUID) error {
	for {
		var seq int64
		var orderID, ownerID uuid.UUID
		var covers int
		var reference string
		err := tx.QueryRow(ctx, `SELECT seq, order_id, owner_id, covers, reference FROM sittings.waitlist WHERE sitting_id = $1 ORDER BY seq LIMIT 1`, sittingID).
			Scan(&seq, &orderID, &ownerID, &covers, &reference)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		taken, err := tx.Exec(ctx, `UPDATE sittings.sittings SET covers_left = covers_left - $2 WHERE id = $1 AND covers_left >= $2`, sittingID, covers)
		if err != nil {
			return err
		}
		if taken.RowsAffected() == 0 {
			return nil // the head does not fit yet: nobody behind it moves
		}
		gone, err := tx.Exec(ctx, `DELETE FROM sittings.waitlist WHERE seq = $1`, seq)
		if err != nil {
			return err
		}
		if gone.RowsAffected() == 0 { // it left the line meanwhile
			if err := s.giveBack(ctx, tx, sittingID, covers); err != nil {
				return err
			}
			continue
		}
		within, err := tx.Exec(ctx, `
			INSERT INTO sittings.guests (sitting_id, owner_id, covers)
			SELECT d.id, $2, $3 FROM sittings.sittings d WHERE d.id = $1 AND d.max_per_guest >= $3
			ON CONFLICT (sitting_id, owner_id) DO UPDATE SET covers = sittings.guests.covers + $3
			WHERE sittings.guests.covers + $3 <= (SELECT max_per_guest FROM sittings.sittings WHERE id = $1)`, sittingID, ownerID, covers)
		if err != nil {
			return err
		}
		if within.RowsAffected() == 0 {
			// The guest picked up other covers while waiting and would now pass the limit: the place is refused.
			if err := s.giveBack(ctx, tx, sittingID, covers); err != nil {
				return err
			}
			var limit int
			if err := tx.QueryRow(ctx, `SELECT max_per_guest FROM sittings.sittings WHERE id = $1`, sittingID).Scan(&limit); err != nil {
				return err
			}
			if err := s.Outbox.Enqueue(ctx, tx, messaging.ShortKey, orderID,
				messaging.CoversShort{OrderID: orderID, Reference: reference, RequestedCovers: covers, CoversLeft: 0, Reason: "cap", PerGuestLimit: limit}); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sittings.cover_holds (order_id, owner_id, sitting_id, covers, status, created_at) VALUES ($1,$2,$3,$4,'Held',now())`,
			orderID, ownerID, sittingID, covers); err != nil {
			return err
		}
		if err := s.Outbox.Enqueue(ctx, tx, messaging.HeldKey, orderID,
			messaging.CoversHeld{OrderID: orderID, OwnerID: ownerID, SittingID: sittingID, Covers: covers, Reference: reference}); err != nil {
			return err
		}
	}
}

func (s *Service) giveBack(ctx context.Context, tx pgx.Tx, sittingID uuid.UUID, covers int) error {
	_, err := tx.Exec(ctx, `UPDATE sittings.sittings SET covers_left = covers_left + $2 WHERE id = $1`, sittingID, covers)
	return err
}

var _ = time.Second
