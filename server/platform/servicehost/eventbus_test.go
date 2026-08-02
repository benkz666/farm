package servicehost

import (
	"strings"
	"testing"
)

func TestOpenEventBusRejectsProcessLocalMemoryBus(t *testing.T) {
	t.Setenv("FARM_BUS", "memory")

	_, err := OpenEventBus(Config{Name: "farm", Environment: devEnvironment}, "farm-0")
	if err == nil || !strings.Contains(err.Error(), `unsupported FARM_BUS "memory"`) {
		t.Fatalf("OpenEventBus() error = %v, want unsupported memory bus", err)
	}
}
