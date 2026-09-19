// Package queue is the Kafka transport for optimisation jobs.
//
// Optimising a query is slow — it executes the query several times, often
// several queries several times, and may build an index — so it cannot happen
// inside an HTTP request. The API validates and enqueues; workers do the work.
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
)

// RetryTiers is the backoff ladder. Each tier is a topic whose consumer does
// nothing but wait, so a failing job cannot stall healthy traffic behind it.
var RetryTiers = []time.Duration{15 * time.Second, 120 * time.Second}

type OptimizeRequest struct {
	JobID      uuid.UUID `json:"job_id"`
	SQL        string    `json:"sql"`
	Attempt    int       `json:"attempt"`
	NotBefore  time.Time `json:"not_before,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	EnqueuedAt time.Time `json:"enqueued_at"`
}

func RetryTopic(base string, attempt int) (string, time.Duration, bool) {
	if attempt < 1 || attempt > len(RetryTiers) {
		return "", 0, false
	}
	d := RetryTiers[attempt-1]
	return fmt.Sprintf("%s.retry.%ds", base, int(d.Seconds())), d, true
}

func AllTopics(base, dlq string) []string {
	out := []string{base, dlq}
	for i := range RetryTiers {
		t, _, _ := RetryTopic(base, i+1)
		out = append(out, t)
	}
	return out
}

type Producer struct{ w *kafka.Writer }

func NewProducer(brokers []string) *Producer {
	return &Producer{w: &kafka.Writer{
		Addr:         kafka.TCP(brokers...),
		Balancer:     &kafka.Hash{},
		RequiredAcks: kafka.RequireAll,
		BatchTimeout: 50 * time.Millisecond,
		MaxAttempts:  5,
	}}
}

func (p *Producer) Close() error { return p.w.Close() }

func (p *Producer) Publish(ctx context.Context, topic string, req OptimizeRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return p.w.WriteMessages(ctx, kafka.Message{
		Topic: topic,
		Key:   []byte(req.JobID.String()),
		Value: body,
		Time:  time.Now(),
	})
}

type Consumer struct{ r *kafka.Reader }

func NewConsumer(brokers []string, topic, group string) *Consumer {
	return &Consumer{r: kafka.NewReader(kafka.ReaderConfig{
		Brokers: brokers, Topic: topic, GroupID: group,
		MinBytes: 1, MaxBytes: 10e6,
		CommitInterval: 0,
		MaxWait:        500 * time.Millisecond,
	})}
}

func (c *Consumer) Close() error { return c.r.Close() }

type Message struct {
	Request OptimizeRequest
	Raw     kafka.Message
}

func (c *Consumer) Fetch(ctx context.Context) (*Message, error) {
	m, err := c.r.FetchMessage(ctx)
	if err != nil {
		return nil, err
	}
	var req OptimizeRequest
	if err := json.Unmarshal(m.Value, &req); err != nil {
		// Commit an undecodable message rather than wedging the partition on it
		// forever; it can never succeed on retry.
		_ = c.r.CommitMessages(ctx, m)
		return nil, fmt.Errorf("undecodable message at offset %d (committed to avoid a poison loop): %w", m.Offset, err)
	}
	return &Message{Request: req, Raw: m}, nil
}

func (c *Consumer) Commit(ctx context.Context, m *Message) error {
	return c.r.CommitMessages(ctx, m.Raw)
}

func EnsureTopics(ctx context.Context, brokers []string, topics []string, partitions int) error {
	conn, err := kafka.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return err
	}
	defer conn.Close()
	controller, err := conn.Controller()
	if err != nil {
		return err
	}
	cc, err := kafka.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", controller.Host, controller.Port))
	if err != nil {
		return err
	}
	defer cc.Close()

	cfgs := make([]kafka.TopicConfig, 0, len(topics))
	for _, t := range topics {
		cfgs = append(cfgs, kafka.TopicConfig{Topic: t, NumPartitions: partitions, ReplicationFactor: 1})
	}
	return cc.CreateTopics(cfgs...)
}
