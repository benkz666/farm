package bus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
)

const kafkaRetryDelay = 100 * time.Millisecond

// KafkaConfig 是 KafkaBus 的连接与消费者组配置。
//
// 同一物理 Farm 实例应使用稳定且唯一的 GroupID，使该实例重启后可以继续提交
// 的消费进度；不同 Farm 实例使用不同 GroupID，使各实例都能收到流，再由后续
// 的分片路由筛选目标事件。
type KafkaConfig struct {
	Brokers     []string
	GroupID     string
	TopicPrefix string
}

// KafkaBus 是 EventBus 的 Kafka 实现。
//
// Publish 使用 key 作为 Kafka message key，Kafka 的 hash 分区器因此能保持同 key
// （通常为 owner_uid）的顺序。Subscribe 只在 handler 成功后提交 offset；失败消息
// 会保留给 Kafka 重试，符合跨农场动作的 at-least-once 语义。
type KafkaBus struct {
	brokers     []string
	groupID     string
	topicPrefix string
	writer      *kafka.Writer
	ctx         context.Context
	cancel      context.CancelFunc

	mu      sync.Mutex
	readers []*kafka.Reader
	closed  bool
	wg      sync.WaitGroup
}

// NewKafkaBus 构造 Kafka EventBus。构造不主动拨号，连接错误会在 Publish 或
// Subscribe 的消费循环中返回/重试，便于进程先于 Kafka 就绪时启动。
func NewKafkaBus(config KafkaConfig) (*KafkaBus, error) {
	brokers := normalizedBrokers(config.Brokers)
	if len(brokers) == 0 {
		return nil, errors.New("bus: kafka brokers are required")
	}
	if strings.TrimSpace(config.GroupID) == "" {
		return nil, errors.New("bus: kafka group id is required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &KafkaBus{
		brokers:     brokers,
		groupID:     strings.TrimSpace(config.GroupID),
		topicPrefix: strings.Trim(strings.TrimSpace(config.TopicPrefix), "."),
		writer: &kafka.Writer{
			Addr:                   kafka.TCP(brokers...),
			Balancer:               &kafka.Hash{},
			RequiredAcks:           kafka.RequireAll,
			AllowAutoTopicCreation: true,
		},
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

// Publish 写入 Kafka。消息的 key 保留为 Kafka key，保证同一主人农场事件有序。
func (b *KafkaBus) Publish(ctx context.Context, topic string, key string, payload []byte) error {
	if err := b.ensureOpen(); err != nil {
		return err
	}
	if strings.TrimSpace(topic) == "" {
		return errors.New("bus: kafka topic is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	return b.writer.WriteMessages(ctx, kafka.Message{
		Topic: b.fullTopic(topic),
		Key:   []byte(key),
		Value: payload,
	})
}

// Subscribe 启动一个异步消费者。handler 成功后才提交 offset；handler 失败时保留
// 当前消息，短暂退避后由 Kafka 重试。
func (b *KafkaBus) Subscribe(ctx context.Context, topic string, handler func(key string, payload []byte) error) error {
	if err := b.ensureOpen(); err != nil {
		return err
	}
	if strings.TrimSpace(topic) == "" {
		return errors.New("bus: kafka topic is required")
	}
	if handler == nil {
		return errors.New("bus: kafka handler is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     b.brokers,
		GroupID:     b.groupID,
		Topic:       b.fullTopic(topic),
		StartOffset: kafka.FirstOffset,
	})

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		_ = reader.Close()
		return ErrBusClosed
	}
	b.readers = append(b.readers, reader)
	b.wg.Add(1)
	b.mu.Unlock()

	go b.consume(ctx, reader, handler)
	return nil
}

// Close 停止所有消费者并关闭 producer。之后 Publish/Subscribe 返回 ErrBusClosed。
func (b *KafkaBus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	readers := append([]*kafka.Reader(nil), b.readers...)
	b.mu.Unlock()

	b.cancel()
	var firstErr error
	for _, reader := range readers {
		if err := reader.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	b.wg.Wait()
	if err := b.writer.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (b *KafkaBus) consume(ctx context.Context, reader *kafka.Reader, handler func(key string, payload []byte) error) {
	defer b.wg.Done()
	defer reader.Close()

	consumeCtx, cancel := context.WithCancel(b.ctx)
	defer cancel()
	stopCaller := context.AfterFunc(ctx, cancel)
	defer stopCaller()

	for {
		message, err := reader.FetchMessage(consumeCtx)
		if err != nil {
			if consumeCtx.Err() != nil {
				return
			}
			if !waitForKafkaRetry(consumeCtx) {
				return
			}
			continue
		}
		if err := handler(string(message.Key), message.Value); err != nil {
			if !waitForKafkaRetry(consumeCtx) {
				return
			}
			continue
		}
		if err := reader.CommitMessages(consumeCtx, message); err != nil {
			if consumeCtx.Err() != nil {
				return
			}
			if !waitForKafkaRetry(consumeCtx) {
				return
			}
		}
	}
}

func (b *KafkaBus) ensureOpen() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrBusClosed
	}
	return nil
}

func (b *KafkaBus) fullTopic(topic string) string {
	if b.topicPrefix == "" {
		return topic
	}
	return fmt.Sprintf("%s.%s", b.topicPrefix, topic)
}

func normalizedBrokers(brokers []string) []string {
	normalized := make([]string, 0, len(brokers))
	for _, broker := range brokers {
		if broker = strings.TrimSpace(broker); broker != "" {
			normalized = append(normalized, broker)
		}
	}
	return normalized
}

func waitForKafkaRetry(ctx context.Context) bool {
	timer := time.NewTimer(kafkaRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
