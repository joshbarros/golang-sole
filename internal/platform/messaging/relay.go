package messaging

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
)

// Relay publishes every module's outbox rows to RabbitMQ (the work queue) and Kafka (the event log), then marks them sent.
// It can publish and crash before the mark; the row then goes out again, which is why every consumer claims an inbox key.
// Any number of replicas can run one: the batch is claimed with SKIP LOCKED.
type Relay struct {
	Pool            *pgxpool.Pool
	Rabbit          *Rabbit
	Kafka           *Kafka
	Outbox          []Outbox
	Log             *slog.Logger
	OutboxRetention time.Duration
	InboxRetention  time.Duration
}

// Run publishes until the context ends.
func (r *Relay) Run(ctx context.Context) {
	if err := r.Kafka.EnsureTopic(ctx); err != nil {
		return
	}
	for ctx.Err() == nil {
		if err := r.runOnce(ctx); err != nil && ctx.Err() == nil {
			r.Log.Warn("outbox relay stopped; reconnecting", "error", err)
			select {
			case <-time.After(relayErrorDelay):
			case <-ctx.Done():
			}
		}
	}
}

func (r *Relay) runOnce(ctx context.Context) error {
	conn, err := r.Rabbit.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	if err := DeclareExchanges(ch); err != nil {
		return err
	}
	if err := ch.Confirm(false); err != nil {
		return err
	}
	closed := conn.NotifyClose(make(chan *amqp.Error, 1))

	publish := func(ctx context.Context, row Row) error {
		conf, err := ch.PublishWithDeferredConfirmWithContext(ctx, Exchange, row.RoutingKey, false, false, amqp.Publishing{
			MessageId:    row.MessageID.String(),
			Type:         row.RoutingKey,
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Body:         row.Payload,
		})
		if err != nil {
			return err
		}
		if conf == nil {
			return fmt.Errorf("publisher confirms are off")
		}
		acked, err := conf.WaitContext(ctx)
		if err != nil {
			return err
		}
		if !acked {
			return fmt.Errorf("rabbitmq nacked %s", row.RoutingKey)
		}
		envelope, err := EnvelopeOf(row)
		if err != nil {
			return err
		}
		return r.Kafka.Append(ctx, row.Key, envelope)
	}

	var lastTrim time.Time
	for ctx.Err() == nil {
		select {
		case e := <-closed:
			return fmt.Errorf("connection closed: %v", e)
		default:
		}
		published := 0
		for _, o := range r.Outbox {
			n, err := o.PublishPending(ctx, r.Pool, publish, outboxBatchSize)
			if err != nil {
				return err
			}
			published += n
			if time.Since(lastTrim) > trimEvery {
				if a, b, err := o.Trim(ctx, r.Pool, r.OutboxRetention, r.InboxRetention); err == nil && a+b > 0 {
					r.Log.Info("trimmed", "schema", o.Schema, "outbox", a, "inbox", b)
				}
			}
		}
		if time.Since(lastTrim) > trimEvery {
			lastTrim = time.Now()
		}
		if published == 0 {
			select {
			case <-time.After(relayIdleDelay):
			case <-ctx.Done():
			}
		}
	}
	return nil
}
