package store

import (
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestMySQLDSNWithInterpolation(t *testing.T) {
	dsn, err := mysqlDSNWithInterpolation("farm:farm@tcp(mysql:3306)/farm?parseTime=true&loc=Local")
	if err != nil {
		t.Fatalf("mysqlDSNWithInterpolation: %v", err)
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if !config.InterpolateParams {
		t.Fatal("InterpolateParams=false, want true")
	}
	if !config.ParseTime || config.DBName != "farm" {
		t.Fatalf("config drift: %#v", config)
	}
}

func TestMySQLDSNWithInterpolationRejectsInvalidDSN(t *testing.T) {
	if _, err := mysqlDSNWithInterpolation("not-a-valid-dsn"); err == nil {
		t.Fatal("invalid DSN unexpectedly accepted")
	}
}
