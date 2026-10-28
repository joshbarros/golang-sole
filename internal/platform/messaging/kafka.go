package messaging

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Kafka is the event log: every outbox row is appended here by the relay, keyed by order id.
type Kafka struct {
	Brokers []string
	Log     *slog.Logger

	producer *kgo.Client
}

// Producer returns the (lazily created) producer.
func (k *Kafka) Producer() (*kgo.Client, error) {
	if k.producer != nil {
		return k.producer, nil
	}
	c, err := kgo.NewClient(
		kgo.SeedBrokers(k.Brokers...),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProduceRequestTimeout(15*time.Second),
		kgo.RecordDeliveryTimeout(15*time.Second),
		kgo.DefaultProduceTopic(KafkaTopic),
	)
	if err != nil {
		return nil, err
	}
	k.producer = c
	return c, nil
}

// EnsureTopic creates the topic (one partition, replication factor one) if it is not there, retrying until the broker answers.
func (k *Kafka) EnsureTopic(ctx context.Context) error {
	for {
		err := k.createTopic(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		k.Log.Warn("kafka not ready", "brokers", k.Brokers, "error", err)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (k *Kafka) createTopic(ctx context.Context) error {
	client, err := kgo.NewClient(kgo.SeedBrokers(k.Brokers...))
	if err != nil {
		return err
	}
	defer client.Close()
	res, err := kadm.NewClient(client).CreateTopics(ctx, 1, 1, nil, KafkaTopic)
	if err != nil {
		return err
	}
	for _, r := range res {
		if r.Err != nil && !errors.Is(r.Err, kerr.TopicAlreadyExists) {
			return r.Err
		}
	}
	return nil
}

// Append writes one record, waiting for every in-sync replica.
func (k *Kafka) Append(ctx context.Context, key string, value []byte) error {
	p, err := k.Producer()
	if err != nil {
		return err
	}
	return p.ProduceSync(ctx, &kgo.Record{Key: []byte(key), Value: value}).FirstErr()
}

// Read returns, in order, the values of the records whose key matches, reading the topic from the start to its end as it
// was when the call began (or until the timeout).
func (k *Kafka) Read(ctx context.Context, key string, timeout time.Duration) ([][]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	admin, err := kgo.NewClient(kgo.SeedBrokers(k.Brokers...))
	if err != nil {
		return nil, err
	}
	defer admin.Close()
	ends, err := kadm.NewClient(admin).ListEndOffsets(ctx, KafkaTopic)
	if err != nil {
		return nil, err
	}
	end, ok := ends.Lookup(KafkaTopic, 0)
	if !ok || end.Err != nil {
		if ok && end.Err != nil {
			return nil, end.Err
		}
		return nil, nil
	}
	if end.Offset == 0 {
		return nil, nil
	}

	consumer, err := kgo.NewClient(
		kgo.SeedBrokers(k.Brokers...),
		kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{KafkaTopic: {0: kgo.NewOffset().AtStart()}}),
	)
	if err != nil {
		return nil, err
	}
	defer consumer.Close()

	var values [][]byte
	for {
		fetches := consumer.PollFetches(ctx)
		if fetches.IsClientClosed() {
			break
		}
		if err := fetches.Err0(); err != nil && ctx.Err() == nil {
			return values, err
		}
		done := false
		fetches.EachRecord(func(r *kgo.Record) {
			if r.Offset >= end.Offset {
				done = true
				return
			}
			if string(r.Key) == key {
				values = append(values, r.Value)
			}
			if r.Offset+1 >= end.Offset {
				done = true
			}
		})
		if done || ctx.Err() != nil {
			break
		}
	}
	return values, nil
}

// Close releases the producer.
func (k *Kafka) Close() {
	if k.producer != nil {
		k.producer.Close()
	}
}
