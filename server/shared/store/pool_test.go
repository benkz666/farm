package store

import (
	"database/sql"
	"testing"
)

func TestConfigureMySQLPoolCapsConnections(t *testing.T) {
	db, err := sql.Open("mysql", "farm:farm@tcp(127.0.0.1:1)/farm")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	configureMySQLPool(db)
	if got := db.Stats().MaxOpenConnections; got != defaultMySQLMaxOpenConns {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, defaultMySQLMaxOpenConns)
	}
}
