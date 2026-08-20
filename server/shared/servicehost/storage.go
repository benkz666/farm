package servicehost

import (
	"context"
	"fmt"
	"time"

	"farm/server/shared/store"
)

// OpenStorage 建立服务需要的共享 MySQL/Redis 连接。
func OpenStorage(ctx context.Context, config Config) (*store.Store, func() error, error) {
	startupContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	storage, closeStorage, err := store.OpenWithPresence(
		startupContext,
		config.MySQLDSN,
		config.RedisAddr,
		Getenv("FARM_PRESENCE_REDIS_ADDR", config.RedisAddr),
		0,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: open storage: %w", config.Name, err)
	}
	return storage, closeStorage, nil
}
