package messaging

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Rabbit dials RabbitMQ, retrying until it answers: a pod can start before the broker does.
type Rabbit struct {
	URL string
	Log *slog.Logger
}

// Dial returns an open connection.
func (r *Rabbit) Dial(ctx context.Context) (*amqp.Connection, error) {
	for {
		conn, err := amqp.DialConfig(r.URL, amqp.Config{Heartbeat: 10 * time.Second, Locale: "en_US"})
		if err == nil {
			return conn, nil
		}
		r.Log.Warn("rabbitmq not ready", "error", err)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// DeclareExchanges declares the three exchanges.
func DeclareExchanges(ch *amqp.Channel) error {
	for _, name := range []string{Exchange, DeadLetterExch, RequeueExchange} {
		if err := ch.ExchangeDeclare(name, "direct", true, false, false, false, nil); err != nil {
			return err
		}
	}
	return nil
}

// DeclareQueue declares a consumer queue with its retry queue (2 s TTL, then back to the main queue) and its dead-letter queue.
func DeclareQueue(ch *amqp.Channel, queue string, routingKeys []string) error {
	if err := DeclareExchanges(ch); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(queue+".dlq", true, false, false, false, nil); err != nil {
		return err
	}
	if err := ch.QueueBind(queue+".dlq", queue, DeadLetterExch, false, nil); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(queue+".retry", true, false, false, false, amqp.Table{
		"x-message-ttl":             int32(RetryDelayMs),
		"x-dead-letter-exchange":    RequeueExchange,
		"x-dead-letter-routing-key": queue,
	}); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(queue, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange":    DeadLetterExch,
		"x-dead-letter-routing-key": queue,
	}); err != nil {
		return err
	}
	if err := ch.QueueBind(queue, queue, RequeueExchange, false, nil); err != nil {
		return err
	}
	for _, key := range routingKeys {
		if err := ch.QueueBind(queue, key, Exchange, false, nil); err != nil {
			return err
		}
	}
	return nil
}

// Consumer is one queue with one handler.
type Consumer struct {
	Queue       string
	RoutingKeys []string
	Prefetch    int
	Handler     Handler
}

// Run consumes until the context ends, reconnecting if the broker goes away.
func (c Consumer) Run(ctx context.Context, rabbit *Rabbit, log *slog.Logger) {
	for ctx.Err() == nil {
		if err := c.runOnce(ctx, rabbit, log); err != nil && ctx.Err() == nil {
			log.Warn("consumer stopped; reconnecting", "queue", c.Queue, "error", err)
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
			}
		}
	}
}

func (c Consumer) runOnce(ctx context.Context, rabbit *Rabbit, log *slog.Logger) error {
	conn, err := rabbit.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	if err := DeclareQueue(ch, c.Queue, c.RoutingKeys); err != nil {
		return err
	}
	if err := ch.Qos(c.Prefetch, 0, false); err != nil {
		return err
	}
	deliveries, err := ch.Consume(c.Queue, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	log.Info("consuming", "queue", c.Queue, "prefetch", c.Prefetch)
	closed := conn.NotifyClose(make(chan *amqp.Error, 1))
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-closed:
			return fmt.Errorf("connection closed: %v", e)
		case d, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("delivery channel closed")
			}
			c.deliver(ctx, ch, d, log)
		}
	}
}

func attemptOf(d amqp.Delivery) int {
	switch v := d.Headers[attemptHeader].(type) {
	case int32:
		return int(v)
	case int64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// deliver runs the handler. A transient failure goes to the retry queue (back to the main queue after 2 s) up to
// MaxAttempts, then to the dead-letter queue. A poison message goes straight to the dead-letter queue.
func (c Consumer) deliver(ctx context.Context, ch *amqp.Channel, d amqp.Delivery, log *slog.Logger) {
	attempt := attemptOf(d)
	err := func() error {
		id, err := parseID(d.MessageId)
		if err != nil {
			return fmt.Errorf("%w: bad message id %q", ErrPoison, d.MessageId)
		}
		// After a trip through the retry queue the broker's routing key is the queue name; the relay stored the original in Type.
		key := d.Type
		if key == "" {
			key = d.RoutingKey
		}
		return c.Handler.Handle(ctx, Delivery{MessageID: id, RoutingKey: key, Body: d.Body})
	}()
	if err == nil {
		_ = d.Ack(false)
		return
	}
	if ctx.Err() != nil {
		_ = d.Nack(false, true)
		return
	}
	if Decide(attempt, err) == ActionDeadLetter {
		if isPoison(err) {
			log.Warn("poison message, dead-lettering", "queue", c.Queue, "reason", err)
		} else {
			log.Error("giving up on a message; dead-lettering", "queue", c.Queue, "attempts", attempt+1, "error", err)
		}
		_ = d.Nack(false, false)
		return
	}
	log.Warn("handler failed; retrying", "queue", c.Queue, "attempt", attempt+1, "error", err)
	pubErr := ch.PublishWithContext(ctx, "", c.Queue+".retry", false, false, amqp.Publishing{
		MessageId:    d.MessageId,
		Type:         d.Type,
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Headers:      amqp.Table{attemptHeader: int32(attempt + 1)},
		Body:         d.Body,
	})
	if pubErr != nil {
		_ = d.Nack(false, true)
		return
	}
	_ = d.Ack(false)
}
