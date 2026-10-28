// Package messaging is the transactional outbox, the inbox, and the RabbitMQ and Kafka plumbing around them.
package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Names of the exchanges, the Kafka topic and the routing keys. They are the contract between the modules.
const (
	Exchange           = "bistro.work"
	DeadLetterExch     = "bistro.dlx"
	RequeueExchange    = "bistro.requeue"
	KafkaTopic         = "bistro.events"
	RetryDelayMs       = 2000
	MaxAttempts        = 3
	ReserveKey         = "covers.reserve"
	ReleaseKey         = "covers.release"
	HeldKey            = "covers.held"
	WaitlistedKey      = "covers.waitlisted"
	ShortKey           = "covers.short"
	ReleasedKey        = "covers.released"
	IssuedKey          = "confirmation.issued"
	RejectedKey        = "confirmation.rejected"
	SittingsQueue      = "sittings.commands"
	ConfirmationsQueue = "confirmations.work"
	OrdersQueue        = "orders.events"
	PoisonToken        = "POISON"
	attemptHeader      = "x-attempt"
	outboxBatchSize    = 50
	relayIdleDelay     = 200 * time.Millisecond
	relayErrorDelay    = time.Second
	trimEvery          = 10 * time.Minute
)

// Delivery is one message handed to a Handler.
type Delivery struct {
	MessageID  uuid.UUID
	RoutingKey string
	Body       []byte
}

// Handler consumes one message. Returning ErrPoison dead-letters it at once; any other error retries it.
type Handler interface {
	Handle(ctx context.Context, d Delivery) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, d Delivery) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, d Delivery) error { return f(ctx, d) }

// ErrPoison marks a message whose rejection is already committed: it must be dead-lettered, not retried.
var ErrPoison = errors.New("poison message")

// OutOfOrderError means the message arrived before the state it depends on; it is retried later.
type OutOfOrderError struct{ Reason string }

func (e *OutOfOrderError) Error() string { return e.Reason }

// IsPoison reports whether the reference carries the poison token (a case-sensitive substring).
func IsPoison(reference string) bool { return strings.Contains(reference, PoisonToken) }

// FailureAction is what a consumer does with a message whose handler failed.
type FailureAction int

// The two things a failed message can meet.
const (
	ActionRetry FailureAction = iota
	ActionDeadLetter
)

// Decide: a poison message is dead-lettered at once; anything else is retried until it has been tried MaxAttempts times.
func Decide(priorAttempts int, err error) FailureAction {
	if errors.Is(err, ErrPoison) {
		return ActionDeadLetter
	}
	if priorAttempts+1 >= MaxAttempts {
		return ActionDeadLetter
	}
	return ActionRetry
}

// ---- the messages ----

// ReserveCovers asks the sitting to hold covers.
type ReserveCovers struct {
	OrderID   uuid.UUID `json:"orderId"`
	OwnerID   uuid.UUID `json:"ownerId"`
	SittingID uuid.UUID `json:"sittingId"`
	Covers    int       `json:"covers"`
	Reference string    `json:"reference"`
	Waitlist  bool      `json:"waitlist"`
}

// CoversHeld says the covers are held.
type CoversHeld struct {
	OrderID   uuid.UUID `json:"orderId"`
	OwnerID   uuid.UUID `json:"ownerId"`
	SittingID uuid.UUID `json:"sittingId"`
	Covers    int       `json:"covers"`
	Reference string    `json:"reference"`
}

// CoversShort says the reserve was refused. Reason: "stock" or "cap".
type CoversShort struct {
	OrderID         uuid.UUID `json:"orderId"`
	Reference       string    `json:"reference"`
	RequestedCovers int       `json:"requestedCovers"`
	CoversLeft      int       `json:"coversLeft"`
	// PerGuestLimit is the cap of the sitting.
	PerGuestLimit int    `json:"perGuestLimit"`
	Reason        string `json:"reason"`
}

// CoversWaitlisted says the sitting was full and the guest asked to wait: the order has a place in the line.
type CoversWaitlisted struct {
	OrderID   uuid.UUID `json:"orderId"`
	Reference string    `json:"reference"`
	Covers    int       `json:"covers"`
	Position  int       `json:"position"`
}

// ReleaseCovers asks the sitting to give held covers back.
type ReleaseCovers struct {
	OrderID uuid.UUID `json:"orderId"`
	Reason  string    `json:"reason"`
}

// CoversReleased says covers (possibly none) were given back.
type CoversReleased struct {
	OrderID uuid.UUID `json:"orderId"`
	Covers  int       `json:"covers"`
	Reason  string    `json:"reason"`
}

// ConfirmationIssued says the confirmation exists.
type ConfirmationIssued struct {
	OrderID uuid.UUID `json:"orderId"`
	Number  string    `json:"number"`
}

// ConfirmationRejected says the confirmation was refused (the poison rule).
type ConfirmationRejected struct {
	OrderID uuid.UUID `json:"orderId"`
	Reason  string    `json:"reason"`
}

// Parse decodes a message body.
func Parse[T any](body []byte) (T, error) {
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return v, err
	}
	return v, nil
}
