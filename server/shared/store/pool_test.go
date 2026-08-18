package store

import (
	"database/sql"
	"runtime"
	"testing"
)

func TestConfigureMySQLPoolCapsConnections(t *testing.T) {
	db, err := sql.Open("mysql", "farm:farm@tcp(127.0.0.1:1)/farm")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	configureMySQLPool(db)
	want, _ := mysqlPoolSizes(runtime.GOMAXPROCS(0))
	if got := db.Stats().MaxOpenConnections; got != want {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, want)
	}
}

func TestMySQLPoolSizesScaleWithProcessors(t *testing.T) {
	oneOpen, oneIdle := mysqlPoolSizes(1)
	twoOpen, twoIdle := mysqlPoolSizes(2)
	if twoOpen != oneOpen*2 || twoIdle != oneIdle*2 {
		t.Fatalf("pool does not scale: one=%d/%d two=%d/%d", oneOpen, oneIdle, twoOpen, twoIdle)
	}
}
