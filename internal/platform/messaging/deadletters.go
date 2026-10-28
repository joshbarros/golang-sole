package messaging

import (
	"context"
	"fmt"
	"net/http"

	"github.com/joshbarros/golang-sole/internal/platform/httpx"
	amqp "github.com/rabbitmq/amqp091-go"
)

// DeadLetter is one message waiting in a dead-letter queue.
type DeadLetter struct {
	RoutingKey string `json:"routingKey"`
	Body       string `json:"body"`
	Deliveries int    `json:"deliveries"`
}

// DeadLetterQueue is what waits behind one consumer queue.
type DeadLetterQueue struct {
	Queue    string       `json:"queue"`
	Count    int          `json:"count"`
	Messages []DeadLetter `json:"messages"`
}

// ReplayResult says how many messages were published again.
type ReplayResult struct {
	Queue    string `json:"queue"`
	Replayed int    `json:"replayed"`
}

// DeadLetters looks inside the dead-letter queues and puts messages back on the work exchange.
type DeadLetters struct {
	Rabbit *Rabbit
	Queues []string
}

func (d *DeadLetters) open(ctx context.Context) (*amqp.Connection, *amqp.Channel, error) {
	conn, err := d.Rabbit.Dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, ch, nil
}

func routingKeyOf(m amqp.Delivery) string {
	if m.Type != "" {
		return m.Type
	}
	return ""
}

func deliveriesOf(m amqp.Delivery) int {
	if deaths, ok := m.Headers["x-death"].([]any); ok && len(deaths) > 0 {
		return len(deaths)
	}
	return 1
}

// List shows what is waiting in each dead-letter queue, with the first few messages. A look, not a take.
func (d *DeadLetters) List(ctx context.Context, peek int) ([]DeadLetterQueue, error) {
	conn, ch, err := d.open(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	out := []DeadLetterQueue{}
	for _, q := range d.Queues {
		dlq := q + ".dlq"
		state, err := ch.QueueDeclarePassive(dlq, true, false, false, false, nil)
		if err != nil {
			// a failed passive declare closes the channel; reopen it
			ch, err = conn.Channel()
			if err != nil {
				return nil, err
			}
			out = append(out, DeadLetterQueue{Queue: q, Messages: []DeadLetter{}})
			continue
		}
		msgs := []DeadLetter{}
		var tags []uint64
		for len(msgs) < peek {
			m, ok, err := ch.Get(dlq, false)
			if err != nil || !ok {
				break
			}
			tags = append(tags, m.DeliveryTag)
			key := routingKeyOf(m)
			if key == "" {
				key = "?"
			}
			msgs = append(msgs, DeadLetter{RoutingKey: key, Body: string(m.Body), Deliveries: deliveriesOf(m)})
		}
		for _, t := range tags {
			_ = ch.Nack(t, false, true)
		}
		out = append(out, DeadLetterQueue{Queue: q, Count: state.Messages + len(tags), Messages: msgs})
	}
	return out, nil
}

// Replay publishes up to max dead-lettered messages of one queue again. A message that still fails for the same reason is
// dead-lettered again after its retries; replay is for fixed causes.
func (d *DeadLetters) Replay(ctx context.Context, queue string, max int) (ReplayResult, error) {
	known := false
	for _, q := range d.Queues {
		known = known || q == queue
	}
	if !known {
		return ReplayResult{}, httpx.Fail(http.StatusNotFound, "There is no queue '%s'.", queue)
	}
	conn, ch, err := d.open(ctx)
	if err != nil {
		return ReplayResult{}, err
	}
	defer conn.Close()
	if err := ch.Confirm(false); err != nil {
		return ReplayResult{}, err
	}
	replayed := 0
	for replayed < max {
		m, ok, err := ch.Get(queue+".dlq", false)
		if err != nil {
			return ReplayResult{}, err
		}
		if !ok {
			break
		}
		key := routingKeyOf(m)
		if key == "" {
			_ = ch.Nack(m.DeliveryTag, false, true)
			break
		}
		conf, err := ch.PublishWithDeferredConfirmWithContext(ctx, Exchange, key, false, false, amqp.Publishing{
			MessageId: m.MessageId, Type: key, ContentType: "application/json", DeliveryMode: amqp.Persistent, Body: m.Body,
		})
		if err != nil {
			return ReplayResult{}, err
		}
		if acked, err := conf.WaitContext(ctx); err != nil || !acked {
			_ = ch.Nack(m.DeliveryTag, false, true)
			return ReplayResult{}, fmt.Errorf("replay was not confirmed: %v", err)
		}
		_ = ch.Ack(m.DeliveryTag, false)
		replayed++
	}
	return ReplayResult{Queue: queue, Replayed: replayed}, nil
}
