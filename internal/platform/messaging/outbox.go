package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Outbox is one module's outbox and inbox, in the module's own schema. A module writes its rows and its outbox rows in
// one local transaction, so no module ever needs another module's tables.
type Outbox struct {
	Schema string
	Module string
}

// Enqueue adds an outbox row to the transaction. Nothing is published here.
func (o Outbox) Enqueue(ctx context.Context, tx pgx.Tx, routingKey string, key uuid.UUID, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s.outbox_messages (message_id, module, routing_key, key, payload, created_at) VALUES ($1,$2,$3,$4,$5,now())`, o.Schema),
		id, o.Module, routingKey, key.String(), body)
	return err
}

// Claim records (consumer, message id) and reports whether this consumer is seeing the message for the first time.
// Call it inside the transaction that does the work, so the claim and the effect commit together.
func (o Outbox) Claim(ctx context.Context, tx pgx.Tx, consumer string, messageID uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s.inbox_messages (consumer, message_id, claimed_at) VALUES ($1,$2,now()) ON CONFLICT DO NOTHING`, o.Schema),
		consumer, messageID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Row is an outbox row as the relay sees it.
type Row struct {
	ID         int64
	MessageID  uuid.UUID
	Module     string
	RoutingKey string
	Key        string
	Payload    []byte
	CreatedAt  time.Time
}

// PublishPending drains a batch the way several API instances can at once: the rows are claimed with
// FOR UPDATE SKIP LOCKED inside a transaction, so two relays never publish the same row at the same time. If one dies
// mid-batch its rows unlock and are published again, which the consumers' inbox absorbs.
func (o Outbox) PublishPending(ctx context.Context, pool *pgxpool.Pool, publish func(context.Context, Row) error, size int) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := tx.Query(ctx, fmt.Sprintf(
		`SELECT id, message_id, module, routing_key, key, payload, created_at FROM %s.outbox_messages
		 WHERE sent_at IS NULL ORDER BY id LIMIT %d FOR UPDATE SKIP LOCKED`, o.Schema, size))
	if err != nil {
		return 0, err
	}
	var batch []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.MessageID, &r.Module, &r.RoutingKey, &r.Key, &r.Payload, &r.CreatedAt); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range batch {
		if err := publish(ctx, r); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.outbox_messages SET sent_at = now() WHERE id = $1`, o.Schema), r.ID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(batch), nil
}

// Trim deletes outbox rows that were sent long ago, and inbox claims older than any redelivery could be. The inbox is
// kept much longer than the outbox because it must outlive the broker's redelivery window.
func (o Outbox) Trim(ctx context.Context, pool *pgxpool.Pool, outboxRetention, inboxRetention time.Duration) (int64, int64, error) {
	now := time.Now()
	a, err := pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.outbox_messages WHERE sent_at IS NOT NULL AND sent_at < $1`, o.Schema), now.Add(-outboxRetention))
	if err != nil {
		return 0, 0, err
	}
	b, err := pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.inbox_messages WHERE claimed_at < $1`, o.Schema), now.Add(-inboxRetention))
	if err != nil {
		return 0, 0, err
	}
	return a.RowsAffected(), b.RowsAffected(), nil
}

// Envelope is what the Kafka event log stores for every outbox row.
type Envelope struct {
	ID         uuid.UUID       `json:"id"`
	Type       string          `json:"type"`
	Module     string          `json:"module"`
	OccurredAt time.Time       `json:"occurredAt"`
	Payload    json.RawMessage `json:"payload"`
}

// EnvelopeOf builds the Kafka value for a row.
func EnvelopeOf(r Row) ([]byte, error) {
	return json.Marshal(Envelope{ID: r.MessageID, Type: r.RoutingKey, Module: r.Module, OccurredAt: r.CreatedAt, Payload: r.Payload})
}
