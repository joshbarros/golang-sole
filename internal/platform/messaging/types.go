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
	Exchange        = "sole.work"
	DeadLetterExch  = "sole.dlx"
	RequeueExchange = "sole.requeue"
	KafkaTopic      = "sole.events"
	RetryDelayMs    = 2000
	MaxAttempts     = 3
	ReserveKey      = "pairs.reserve"
	ReleaseKey      = "pairs.release"
	HeldKey         = "pairs.held"
	ShortKey        = "pairs.short"
	EnteredKey      = "pairs.entered"
	ReleasedKey     = "pairs.released"
	IssuedKey       = "claim.issued"
	RejectedKey     = "claim.rejected"
	RafflesQueue    = "raffles.commands"
	ClaimsQueue     = "claims.work"
	OrdersQueue     = "orders.events"
	PoisonToken     = "POISON"
	attemptHeader   = "x-attempt"
	outboxBatchSize = 50
	relayIdleDelay  = 200 * time.Millisecond
	relayErrorDelay = time.Second
	trimEvery       = 10 * time.Minute
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

// ReservePairs asks the raffle to record an entry.
type ReservePairs struct {
	OrderID   uuid.UUID `json:"orderId"`
	OwnerID   uuid.UUID `json:"ownerId"`
	RaffleID  uuid.UUID `json:"raffleId"`
	Pairs     int       `json:"pairs"`
	Reference string    `json:"reference"`
}

// PairsEntered says the entry was recorded.
type PairsEntered struct {
	OrderID uuid.UUID `json:"orderId"`
	DrawAt  time.Time `json:"drawAt"`
}

// PairsHeld says the entry won and its pairs are held.
type PairsHeld struct {
	OrderID   uuid.UUID `json:"orderId"`
	OwnerID   uuid.UUID `json:"ownerId"`
	RaffleID  uuid.UUID `json:"raffleId"`
	Pairs     int       `json:"pairs"`
	Reference string    `json:"reference"`
}

// PairsShort says the entry did not make it. Reason: "closed", "already" or "drawn".
type PairsShort struct {
	OrderID        uuid.UUID `json:"orderId"`
	Reference      string    `json:"reference"`
	RequestedPairs int       `json:"requestedPairs"`
	PairsLeft      int       `json:"pairsLeft"`
	Reason         string    `json:"reason"`
	Rank           int       `json:"rank"`
	Entries        int       `json:"entries"`
}

// ReleasePairs asks the raffle to withdraw an entry or give held pairs back.
type ReleasePairs struct {
	OrderID uuid.UUID `json:"orderId"`
	Reason  string    `json:"reason"`
}

// PairsReleased says pairs (possibly none) were given back.
type PairsReleased struct {
	OrderID uuid.UUID `json:"orderId"`
	Pairs   int       `json:"pairs"`
	Reason  string    `json:"reason"`
}

// ClaimIssued says the claim exists.
type ClaimIssued struct {
	OrderID uuid.UUID `json:"orderId"`
	Number  string    `json:"number"`
}

// ClaimRejected says the claim was refused (the poison rule).
type ClaimRejected struct {
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
