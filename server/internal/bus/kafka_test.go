package bus

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestKafkaBusRejectsMissingBrokers(t *testing.T) {
	_, err := NewKafkaBus(KafkaConfig{GroupID: "farm-test"})
	if err == nil {
		t.Fatal("缺少 broker 时应拒绝构造 KafkaBus")
	}
	if !strings.Contains(err.Error(), "brokers") {
		t.Fatalf("错误应说明 brokers 配置无效，got %v", err)
	}
}

func TestKafkaBusRoundTrip(t *testing.T) {
	brokers := strings.TrimSpace(os.Getenv("FARM_KAFKA_BROKERS"))
	if brokers == "" {
		t.Skip("未设置 FARM_KAFKA_BROKERS，跳过 Kafka 集成测试")
	}

	const topic = "bus.integration"
	key := "owner:42"
	payload := []byte(`{"req_id":1}`)
	bus, err := NewKafkaBus(KafkaConfig{
		Brokers: strings.Split(brokers, ","),
		GroupID: "bus-test-" + strings.ReplaceAll(t.Name(), "/", "-"),
	})
	if err != nil {
		t.Fatalf("new KafkaBus: %v", err)
	}
	t.Cleanup(func() {
		if err := bus.Close(); err != nil {
			t.Errorf("close KafkaBus: %v", err)
		}
	})

	got := make(chan struct {
		key     string
		payload []byte
	}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := bus.Subscribe(ctx, topic, func(gotKey string, gotPayload []byte) error {
		got <- struct {
			key     string
			payload []byte
		}{key: gotKey, payload: gotPayload}
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := bus.Publish(ctx, topic, key, payload); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case message := <-got:
		if message.key != key {
			t.Fatalf("key: want %q got %q", key, message.key)
		}
		if !bytes.Equal(message.payload, payload) {
			t.Fatalf("payload: want %q got %q", payload, message.payload)
		}
	case <-ctx.Done():
		t.Fatalf("等待 Kafka 消息: %v", ctx.Err())
	}
}
