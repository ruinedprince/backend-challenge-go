package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"

	"github.com/ruinedprince/backend-challenge-go/internal/core"
	"github.com/ruinedprince/backend-challenge-go/internal/domain"
)

type SQSConsumer struct {
	client            *sqs.Client
	processor         *core.Processor
	logger            *slog.Logger
	cancel            context.CancelFunc
	done              chan struct{}
	queueURL          string
	queueName         string
	afterCommit       func(context.Context, sqsTypesMessage)
	afterDelete       func(context.Context, sqsTypesMessage)
	beforeProcess     func(context.Context, sqsTypesMessage)
	visibilityTimeout int32
}

func NewSQSConsumer(client *sqs.Client, processor *core.Processor, logger *slog.Logger) *SQSConsumer {
	return &SQSConsumer{
		client:            client,
		processor:         processor,
		logger:            logger,
		done:              make(chan struct{}),
		queueName:         "wager-transactions.fifo",
		afterCommit:       func(context.Context, sqsTypesMessage) {},
		afterDelete:       func(context.Context, sqsTypesMessage) {},
		beforeProcess:     func(context.Context, sqsTypesMessage) {},
		visibilityTimeout: 30,
	}
}
func RegisterSQSConsumer(lifecycle fx.Lifecycle, consumer *SQSConsumer) {
	lifecycle.Append(fx.Hook{OnStart: consumer.Start, OnStop: consumer.Stop})
}
func (c *SQSConsumer) Start(ctx context.Context) error {
	out, err := c.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(c.queueName)})
	if err != nil {
		return err
	}
	c.queueURL = aws.ToString(out.QueueUrl)
	runCtx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go c.loop(runCtx)
	return nil
}
func (c *SQSConsumer) Stop(ctx context.Context) error {
	if c.cancel != nil {
		c.cancel()
	}
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *SQSConsumer) loop(ctx context.Context) {
	defer close(c.done)
	for {
		if ctx.Err() != nil {
			return
		}
		out, err := c.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(c.queueURL), MaxNumberOfMessages: 1, WaitTimeSeconds: 5, VisibilityTimeout: c.visibilityTimeout, MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount}})
		if err != nil {
			if ctx.Err() == nil {
				c.logger.Error("receive SQS message", "error", err)
			}
			continue
		}
		for _, message := range out.Messages {
			if err := c.handle(ctx, message); err != nil {
				c.logger.Error("process SQS message", "messageId", aws.ToString(message.MessageId), "error", err)
				c.scheduleRetry(ctx, message)
				continue
			}
			// The durable transaction, including inbox completion, is already committed.
			// Keeping this seam here lets the integration test simulate a process death
			// in the narrow window before DeleteMessage.
			c.afterCommit(ctx, message)
			if ctx.Err() != nil {
				return
			}
			_, err = c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(c.queueURL), ReceiptHandle: message.ReceiptHandle})
			if err != nil {
				c.logger.Error("delete processed SQS message", "error", err)
				continue
			}
			c.afterDelete(ctx, message)
		}
	}
}

func (c *SQSConsumer) scheduleRetry(ctx context.Context, message sqsTypesMessage) {
	visibilityTimeout := retryVisibilityTimeout(message.Attributes["ApproximateReceiveCount"])
	if _, err := c.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(c.queueURL),
		ReceiptHandle:     message.ReceiptHandle,
		VisibilityTimeout: visibilityTimeout,
	}); err != nil && ctx.Err() == nil {
		c.logger.Error("schedule SQS retry", "messageId", aws.ToString(message.MessageId), "error", err)
	}
}

func retryVisibilityTimeout(receiveCount string) int32 {
	count, err := strconv.Atoi(receiveCount)
	if err != nil || count < 1 {
		count = 1
	}
	if count > 5 {
		count = 5
	}
	seconds := int32(30) * (1 << (count - 1))
	if seconds > 300 {
		return 300
	}
	return seconds
}
func (c *SQSConsumer) handle(ctx context.Context, message sqsTypesMessage) error {
	var envelope struct {
		MessageID string `json:"messageId"`
		Type      string `json:"type"`
		Data      struct {
			ProviderID                     string                 `json:"providerId"`
			ExternalTransactionID          string                 `json:"externalTransactionId"`
			IdempotencyKey                 string                 `json:"idempotencyKey"`
			PlayerID                       string                 `json:"playerId"`
			WalletID                       string                 `json:"walletId"`
			RoundID                        string                 `json:"roundId"`
			GameID                         string                 `json:"gameId"`
			ReferenceExternalTransactionID string                 `json:"referenceExternalTransactionId"`
			Kind                           domain.TransactionType `json:"kind"`
			Type                           domain.TransactionType `json:"type"`
			Money                          struct {
				Amount   string `json:"amount"`
				Currency string `json:"currency"`
			} `json:"money"`
		} `json:"data"`
	}
	body := aws.ToString(message.Body)
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		return err
	}
	money, err := domain.ParseExternalMoney(envelope.Data.Money.Amount, envelope.Data.Money.Currency)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(body))
	kind := envelope.Data.Kind
	if kind == "" {
		kind = envelope.Data.Type
	}
	c.beforeProcess(ctx, message)
	_, _, err = c.processor.ProcessInbox(ctx, "wager-transactions", envelope.MessageID, hex.EncodeToString(sum[:]), core.TransactionRequest{ProviderID: envelope.Data.ProviderID, ExternalTransactionID: envelope.Data.ExternalTransactionID, IdempotencyKey: envelope.Data.IdempotencyKey, PlayerID: envelope.Data.PlayerID, WalletID: envelope.Data.WalletID, RoundID: envelope.Data.RoundID, GameID: envelope.Data.GameID, Type: kind, Money: money, ReferenceExternalTransactionID: envelope.Data.ReferenceExternalTransactionID, OccurredAt: time.Now().UTC()})
	return err
}

// local alias keeps the handler testable without exposing AWS internals.
type sqsTypesMessage = types.Message
