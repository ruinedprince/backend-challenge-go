package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
)

const (
	outboxQueueName = "wager-events.fifo"
	outboxLease     = 30 * time.Second
)

// OutboxPublisher claims committed events before sending them. A lease makes a
// crashed publisher recoverable; a repeated send intentionally uses the same
// event ID so downstream consumers can deduplicate at-least-once delivery.
type OutboxPublisher struct {
	pool     *pgxpool.Pool
	client   *sqs.Client
	logger   *slog.Logger
	cancel   context.CancelFunc
	done     chan struct{}
	queueURL string
}

type outboxEvent struct {
	ID          string
	AggregateID string
	EventType   string
	Payload     []byte
	OccurredAt  time.Time
}

func NewOutboxPublisher(pool *pgxpool.Pool, client *sqs.Client, logger *slog.Logger) *OutboxPublisher {
	return &OutboxPublisher{pool: pool, client: client, logger: logger, done: make(chan struct{})}
}

func RegisterOutboxPublisher(lifecycle fx.Lifecycle, publisher *OutboxPublisher) {
	lifecycle.Append(fx.Hook{OnStart: publisher.Start, OnStop: publisher.Stop})
}

func (p *OutboxPublisher) Start(ctx context.Context) error {
	queue, err := p.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(outboxQueueName)})
	if err != nil {
		return fmt.Errorf("get outbox destination queue: %w", err)
	}
	p.queueURL = aws.ToString(queue.QueueUrl)
	runCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go p.loop(runCtx)
	return nil
}

func (p *OutboxPublisher) Stop(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *OutboxPublisher) loop(ctx context.Context) {
	defer close(p.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := p.publishAvailable(ctx); err != nil && ctx.Err() == nil {
			p.logger.Error("publish outbox events", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *OutboxPublisher) publishAvailable(ctx context.Context) error {
	events, err := p.claim(ctx, 10)
	if err != nil {
		return err
	}
	for _, event := range events {
		if err := p.publishOne(ctx, event); err != nil {
			if releaseErr := p.releaseWithBackoff(ctx, event.ID); releaseErr != nil {
				return fmt.Errorf("publish event %s: %w (schedule retry: %v)", event.ID, err, releaseErr)
			}
			p.logger.Warn("outbox event scheduled for retry", "eventId", event.ID, "error", err)
		}
	}
	return nil
}

func (p *OutboxPublisher) claim(ctx context.Context, limit int32) ([]outboxEvent, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin outbox claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
        WITH candidates AS (
            SELECT id FROM outbox_events
            WHERE published_at IS NULL AND next_attempt_at <= now()
              AND (claimed_at IS NULL OR claimed_at < now() - $1::interval)
            ORDER BY next_attempt_at, created_at
            FOR UPDATE SKIP LOCKED LIMIT $2
        )
        UPDATE outbox_events o SET claimed_at = now(), attempts = attempts + 1
        FROM candidates c WHERE o.id = c.id
        RETURNING o.id, o.aggregate_id, o.event_type, o.payload, o.occurred_at
    `, outboxLease.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbox events: %w", err)
	}
	defer rows.Close()
	var events []outboxEvent
	for rows.Next() {
		var event outboxEvent
		if err := rows.Scan(&event.ID, &event.AggregateID, &event.EventType, &event.Payload, &event.OccurredAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit outbox claim: %w", err)
	}
	return events, nil
}

func (p *OutboxPublisher) publishOne(ctx context.Context, event outboxEvent) error {
	var payload struct {
		CorrelationID string          `json:"correlationId"`
		Data          json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("decode immutable payload: %w", err)
	}
	if len(payload.Data) == 0 || string(payload.Data) == "null" {
		return fmt.Errorf("decode immutable payload: missing typed data")
	}
	body, err := json.Marshal(struct {
		EventID       string          `json:"eventId"`
		EventType     string          `json:"eventType"`
		AggregateID   string          `json:"aggregateId"`
		CorrelationID string          `json:"correlationId"`
		OccurredAt    time.Time       `json:"occurredAt"`
		Version       int             `json:"version"`
		Data          json.RawMessage `json:"data"`
	}{event.ID, event.EventType, event.AggregateID, payload.CorrelationID, event.OccurredAt.UTC(), 1, payload.Data})
	if err != nil {
		return fmt.Errorf("encode outgoing event: %w", err)
	}
	if _, err = p.client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(p.queueURL), MessageBody: aws.String(string(body)), MessageGroupId: aws.String(event.AggregateID), MessageDeduplicationId: aws.String(event.ID)}); err != nil {
		return fmt.Errorf("send SQS message: %w", err)
	}
	command, err := p.pool.Exec(ctx, `UPDATE outbox_events SET published_at = now(), claimed_at = NULL WHERE id = $1 AND published_at IS NULL`, event.ID)
	if err != nil {
		return fmt.Errorf("confirm outbox publication: %w", err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("confirm outbox publication: event %s was not pending", event.ID)
	}
	return nil
}

func (p *OutboxPublisher) releaseWithBackoff(ctx context.Context, eventID string) error {
	_, err := p.pool.Exec(ctx, `
        UPDATE outbox_events
        SET claimed_at = NULL,
            next_attempt_at = now() + (LEAST(300, POWER(2, LEAST(attempts, 8))::int) * interval '1 second')
        WHERE id = $1 AND published_at IS NULL
    `, eventID)
	return err
}
