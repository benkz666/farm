package servicehost

import (
	"fmt"
	"strings"

	"farm/server/platform/bus"
)

// OpenEventBus 为 Gateway/Farm 创建跨农场事件总线。
// 五服务架构的跨进程事件固定通过 Kafka 传递；MemoryBus 仅供单元测试直接注入。
func OpenEventBus(config Config, instanceID string) (bus.EventBus, error) {
	kind := strings.ToLower(getenv("FARM_BUS", "kafka"))
	if kind != "kafka" {
		return nil, fmt.Errorf("%s: unsupported FARM_BUS %q", config.Name, kind)
	}
	brokers := splitCSV(getenv("FARM_KAFKA_BROKERS", "127.0.0.1:9094"))
	eventBus, err := bus.NewKafkaBus(bus.KafkaConfig{
		Brokers: brokers,
		GroupID: "farm-cross-" + config.Name + "-" + strings.TrimSpace(instanceID),
	})
	if err != nil {
		return nil, fmt.Errorf("%s: create event bus: %w", config.Name, err)
	}
	return eventBus, nil
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			values = append(values, part)
		}
	}
	return values
}
