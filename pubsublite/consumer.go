// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package pubsublite

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/pubsublite/pscompat"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.18.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/option"

	"github.com/elastic/apm-data/model"
	apmqueue "github.com/elastic/apm-queue"
	"github.com/elastic/apm-queue/pubsublite/internal/telemetry"
	"github.com/elastic/apm-queue/queuecontext"
)

// Decoder decodes a []byte into a model.APMEvent
type Decoder interface {
	// Decode decodes an encoded model.APM Event into its struct form.
	Decode([]byte, *model.APMEvent) error
}

// ConsumerConfig defines the configuration for the PubSub Lite consumer.
type ConsumerConfig struct {
	// Region is the GCP region for the producer.
	Region string
	// Project is the GCP project for the producer.
	Project string
	// Topics holds Pub/Sub Lite topics from which messages will be consumed.
	Topics []apmqueue.Topic
	// Decoder holds an encoding.Decoder for decoding events.
	Decoder Decoder
	// Logger to use for any errors.
	Logger *zap.Logger
	// Processor that will be used to process each event individually.
	// Processor may be called from multiple goroutines and needs to be
	// safe for concurrent use.
	Processor model.BatchProcessor
	// Delivery mechanism to use to acknowledge the messages.
	// AtMostOnceDeliveryType and AtLeastOnceDeliveryType are supported.
	Delivery   apmqueue.DeliveryType
	ClientOpts []option.ClientOption

	// TracerProvider allows specifying a custom otel tracer provider.
	// Defaults to the global one.
	TracerProvider trace.TracerProvider
}

// Subscription represents a PubSub Lite subscription.
type Subscription struct {
	// Project where the subscription is located.
	Project string
	// Region where the subscription is located.
	Region string
	// Name/ID of the subscription.
	Name string
}

func (s Subscription) String() string {
	return fmt.Sprintf("projects/%s/locations/%s/subscriptions/%s",
		s.Project, s.Region, s.Name,
	)
}

// Validate ensures the configuration is valid, otherwise, returns an error.
func (cfg ConsumerConfig) Validate() error {
	var errs []error
	if len(cfg.Topics) == 0 {
		errs = append(errs,
			errors.New("pubsublite: at least one topic must be set"),
		)
	}
	if cfg.Project == "" {
		errs = append(errs, errors.New("pubsublite: project must be set"))
	}
	if cfg.Region == "" {
		errs = append(errs, errors.New("pubsublite: region must be set"))
	}
	if cfg.Decoder == nil {
		errs = append(errs, errors.New("pubsublite: decoder must be set"))
	}
	if cfg.Logger == nil {
		errs = append(errs, errors.New("pubsublite: logger must be set"))
	}
	if cfg.Processor == nil {
		errs = append(errs, errors.New("pubsublite: processor must be set"))
	}
	switch cfg.Delivery {
	case apmqueue.AtLeastOnceDeliveryType:
	case apmqueue.AtMostOnceDeliveryType:
	default:
		errs = append(errs, errors.New("pubsublite: delivery is not valid"))
	}
	return errors.Join(errs...)
}

// Consumer receives PubSub Lite messages from a existing subscription(s). The
// underlying library processes messages concurrently per subscription and
// partition.
type Consumer struct {
	mu             sync.Mutex
	cfg            ConsumerConfig
	consumers      []*consumer
	stopSubscriber context.CancelFunc
	tracer         trace.Tracer
}

// NewConsumer creates a new consumer instance for a single subscription.
func NewConsumer(ctx context.Context, cfg ConsumerConfig) (*Consumer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("pubsublite: invalid consumer config: %w", err)
	}
	settings := pscompat.ReceiveSettings{
		// Pub/Sub Lite does not have a concept of 'nack'. If the nack handler
		// implementation returns nil, the message is acknowledged. If an error
		// is returned, it's considered a fatal error and the client terminates.
		// In Pub/Sub Lite, only a single subscriber for a given subscription
		// is connected to any partition at a time, and there is no other client
		// that may be able to handle messages.
		NackHandler: func(msg *pubsub.Message) error {
			// TODO(marclop) DLQ?
			partition, offset := partitionOffset(msg.ID)
			cfg.Logger.Error("handling nacked message",
				zap.Int("partition", partition),
				zap.Int64("offset", offset),
				zap.Any("attributes", msg.Attributes),
			)
			return nil // nil is returned to avoid terminating the subscriber.
		},
	}
	consumers := make([]*consumer, 0, len(cfg.Topics))
	cfg.Logger = cfg.Logger.Named("pubsublite")
	for _, topic := range cfg.Topics {
		subscription := Subscription{
			Name:    string(topic),
			Project: cfg.Project,
			Region:  cfg.Region,
		}
		client, err := pscompat.NewSubscriberClientWithSettings(
			ctx, subscription.String(), settings, cfg.ClientOpts...,
		)
		if err != nil {
			return nil, fmt.Errorf("pubsublite: failed creating consumer: %w", err)
		}
		consumers = append(consumers, &consumer{
			SubscriberClient: client,
			delivery:         cfg.Delivery,
			processor:        cfg.Processor,
			decoder:          cfg.Decoder,
			logger: cfg.Logger.With(
				zap.String("subscription", string(topic)),
				zap.String("region", cfg.Region),
				zap.String("project", cfg.Project),
			),
			telemetryAttributes: []attribute.KeyValue{
				semconv.MessagingSourceNameKey.String(string(topic)),
				semconv.CloudRegion(cfg.Region),
				semconv.CloudAccountID(cfg.Project),
			},
		})
	}

	tracerProvider := cfg.TracerProvider
	if tracerProvider == nil {
		tracerProvider = otel.GetTracerProvider()
	}

	return &Consumer{
		cfg:       cfg,
		consumers: consumers,
		tracer:    tracerProvider.Tracer("pubsublite"),
	}, nil
}

// Close closes the consumer. Once the consumer is closed, it can't be re-used.
func (c *Consumer) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopSubscriber()
	return nil
}

// Run executes the consumer in a blocking manner. It should only be called once,
// any subsequent calls will return an error.
func (c *Consumer) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.stopSubscriber != nil {
		c.mu.Unlock()
		return errors.New("pubsublite: consumer already started")
	}
	ctx, c.stopSubscriber = context.WithCancel(ctx)
	c.mu.Unlock()

	g, ctx := errgroup.WithContext(ctx)
	for _, consumer := range c.consumers {
		consumer := consumer
		g.Go(func() error {
			for {
				err := consumer.Receive(ctx, telemetry.Consumer(
					c.tracer,
					consumer.processMessage,
					consumer.telemetryAttributes,
				))
				// Keep attempting to receive until a fatal error is received.
				if errors.Is(err, pscompat.ErrBackendUnavailable) {
					continue
				}
				return err
			}
		})
	}
	return g.Wait()
}

// Healthy returns an error if the consumer isn't healthy.
func (c *Consumer) Healthy(ctx context.Context) error {
	return nil // TODO(marclop)
}

// consumer wraps a PubSub Lite SubscriberClient.
type consumer struct {
	*pscompat.SubscriberClient
	logger              *zap.Logger
	delivery            apmqueue.DeliveryType
	processor           model.BatchProcessor
	decoder             Decoder
	telemetryAttributes []attribute.KeyValue
	failed              sync.Map
}

func (c *consumer) processMessage(ctx context.Context, msg *pubsub.Message) {
	var event model.APMEvent
	if err := c.decoder.Decode(msg.Data, &event); err != nil {
		defer msg.Nack()
		partition, offset := partitionOffset(msg.ID)
		c.logger.Error("unable to decode message.Data into model.APMEvent",
			zap.Error(err),
			zap.ByteString("message.value", msg.Data),
			zap.Int64("offset", offset),
			zap.Int("partition", partition),
			zap.Any("headers", msg.Attributes),
		)
		return
	}
	batch := model.Batch{event}
	ctx = queuecontext.WithMetadata(ctx, msg.Attributes)
	var err error
	switch c.delivery {
	case apmqueue.AtMostOnceDeliveryType:
		msg.Ack()
	case apmqueue.AtLeastOnceDeliveryType:
		defer func() {
			// If processing fails, the message will not be Nacked until the 3rd
			// delivery, otherwise, ack the message.
			if err != nil {
				attempt := int(1)
				if a, ok := c.failed.LoadOrStore(msg.ID, attempt); ok {
					attempt += a.(int)
				}
				if attempt > 2 {
					msg.Nack()
					c.failed.Delete(msg.ID)
					return
				}
				c.failed.Store(msg.ID, attempt)
				return
			}
			partition, offset := partitionOffset(msg.ID)
			c.logger.Info("processed previously failed event",
				zap.Int64("offset", offset),
				zap.Int("partition", partition),
				zap.Any("headers", msg.Attributes),
			)
			msg.Ack()
			c.failed.Delete(msg.ID)
		}()
	}
	if err = c.processor.ProcessBatch(ctx, &batch); err != nil {
		partition, offset := partitionOffset(msg.ID)
		c.logger.Error("unable to process event",
			zap.Error(err),
			zap.Int64("offset", offset),
			zap.Int("partition", partition),
			zap.Any("headers", msg.Attributes),
		)
		return
	}
}

// Parses the message partition and offset. If the metadata can't be parsed,
// zero values are returned.
func partitionOffset(id string) (partition int, offset int64) {
	if meta, _ := pscompat.ParseMessageMetadata(id); meta != nil {
		partition, offset = meta.Partition, meta.Offset
	}
	return
}
