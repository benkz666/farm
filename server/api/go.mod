module farm/server/api

go 1.25.0

require farm/server/platform v0.0.0

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-sql-driver/mysql v1.10.0 // indirect
	github.com/redis/go-redis/v9 v9.21.0 // indirect
	github.com/stretchr/testify v1.10.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
)

replace farm/server/platform => ../platform
