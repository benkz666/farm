package servicehost

import "testing"

func TestLoadConfigUsesServiceSpecificAddress(t *testing.T) {
	t.Setenv("FARM_INTERNAL_TOKEN", "secret")
	t.Setenv("FARM_GATEWAY_HTTP_ADDR", ":1234")
	t.Setenv("FARM_HTTP_ADDR", ":9999") // 旧单进程变量必须被忽略。

	config, err := LoadConfig("gateway", ":9002", "off")
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.HTTPAddr != ":1234" {
		t.Fatalf("HTTPAddr = %q", config.HTTPAddr)
	}
}

func TestLoadConfigRejectsMissingInternalToken(t *testing.T) {
	t.Setenv("FARM_INTERNAL_TOKEN", "")
	if _, err := LoadConfig("worker", ":9005", "off"); err == nil {
		t.Fatal("LoadConfig() accepted an empty internal token")
	}
}
