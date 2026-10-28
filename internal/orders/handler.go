package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/joshbarros/golang-bistro/internal/platform/messaging"
	"github.com/joshbarros/golang-bistro/internal/platform/pgdb"
)

// MarkOutcome is what a mark did.
type MarkOutcome int

// The ways a mark can end.
const (
	Marked MarkOutcome = iota
	Skipped
	Duplicate
)

// Handle consumes the events that move an order forward. Each step runs only from the status it expects.
func (s *Service) Handle(ctx context.Context, d messaging.Delivery) error {
	poison := func(err error) error { return fmt.Errorf("%w: %v", messaging.ErrPoison, err) }
	switch d.RoutingKey {
	case messaging.HeldKey:
		m, err := messaging.Parse[messaging.CoversHeld](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkHeld(ctx, d.MessageID, m.OrderID)
		return err
	case messaging.WaitlistedKey:
		m, err := messaging.Parse[messaging.CoversWaitlisted](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkWaitlisted(ctx, d.MessageID, m)
		return err
	case messaging.ShortKey:
		m, err := messaging.Parse[messaging.CoversShort](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkShort(ctx, d.MessageID, m)
		return err
	case messaging.IssuedKey:
		m, err := messaging.Parse[messaging.ConfirmationIssued](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkSold(ctx, d.MessageID, m, s.FinalMarkDelay)
		return err
	case messaging.RejectedKey:
		m, err := messaging.Parse[messaging.ConfirmationRejected](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkRejected(ctx, d.MessageID, m)
		return err
	case messaging.ReleasedKey:
		m, err := messaging.Parse[messaging.CoversReleased](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkReleased(ctx, d.MessageID, m)
		return err
	}
	return fmt.Errorf("orders does not handle %q", d.RoutingKey)
}

// step runs update inside one transaction with the inbox confirmation. When update changes nothing and the order is still waiting
// for an earlier mark (Requested), the message arrived too early: it is retried, not dropped.
func (s *Service) step(ctx context.Context, messageID uuid.UUID, orderForOrderCheck *uuid.UUID, update func(pgx.Tx) (int64, error)) (MarkOutcome, error) {
	outcome := Marked
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		changed, err := update(tx)
		if err != nil {
			return err
		}
		if changed == 0 && orderForOrderCheck != nil {
			var status string
			err := tx.QueryRow(ctx, `SELECT status FROM orders.orders WHERE id = $1`, *orderForOrderCheck).Scan(&status)
			if err == nil && status == Requested || status == Waitlisted {
				return &messaging.OutOfOrderError{Reason: "The final mark arrived before the Held mark."}
			}
		}
		if changed == 0 {
			outcome = Skipped
		}
		return nil
	})
	return outcome, err
}

// MarkHeld: the covers are held.
func (s *Service) MarkHeld(ctx context.Context, messageID, orderID uuid.UUID) (MarkOutcome, error) {
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET status = 'Held', reason = NULL, updated_at = now() WHERE id = $1 AND status IN ('Requested','Waitlisted')`, orderID)
		return tag.RowsAffected(), err
	})
}

// ShortReason words why the reserve was refused.
func ShortReason(m messaging.CoversShort) string {
	if m.Reason == "cap" {
		return fmt.Sprintf("Cap: at most %d covers per guest in this sitting.", m.PerGuestLimit)
	}
	return fmt.Sprintf("Short: %d covers wanted, %d left.", m.RequestedCovers, m.CoversLeft)
}

// MarkWaitlisted is only from Requested: if the order was already promoted, the Held mark got here first and this one is moot.
func (s *Service) MarkWaitlisted(ctx context.Context, messageID uuid.UUID, m messaging.CoversWaitlisted) (MarkOutcome, error) {
	reason := fmt.Sprintf("Waitlisted: number %d in line for %d covers.", m.Position, m.Covers)
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET status = 'Waitlisted', reason = $2, updated_at = now() WHERE id = $1 AND status = 'Requested'`, m.OrderID, reason)
		return tag.RowsAffected(), err
	})
}

// MarkShort: the cap or the stock said no.
func (s *Service) MarkShort(ctx context.Context, messageID uuid.UUID, m messaging.CoversShort) (MarkOutcome, error) {
	reason := ShortReason(m)
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET status = 'Rejected', reason = $2, updated_at = now() WHERE id = $1 AND status IN ('Requested','Waitlisted')`, m.OrderID, reason)
		return tag.RowsAffected(), err
	})
}

// MarkSold is the final mark. If a refund got there first the row is no longer Held and nothing changes.
func (s *Service) MarkSold(ctx context.Context, messageID uuid.UUID, m messaging.ConfirmationIssued, delay time.Duration) (MarkOutcome, error) {
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return Skipped, ctx.Err()
		}
	}
	return s.step(ctx, messageID, &m.OrderID, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET status = 'Sold', confirmation_number = $2, updated_at = now() WHERE id = $1 AND status = 'Held'`, m.OrderID, m.Number)
		return tag.RowsAffected(), err
	})
}

// MarkRejected: the confirmation was refused (poison).
func (s *Service) MarkRejected(ctx context.Context, messageID uuid.UUID, m messaging.ConfirmationRejected) (MarkOutcome, error) {
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET status = 'Rejected', reason = $2, updated_at = now() WHERE id = $1 AND status IN ('Held','Requested')`, m.OrderID, m.Reason)
		return tag.RowsAffected(), err
	})
}

// MarkReleased records that the covers came back.
func (s *Service) MarkReleased(ctx context.Context, messageID uuid.UUID, m messaging.CoversReleased) (MarkOutcome, error) {
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET covers_released_at = now() WHERE id = $1 AND covers_released_at IS NULL`, m.OrderID)
		return tag.RowsAffected(), err
	})
}

var _ = errors.New
