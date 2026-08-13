package main

import (
	"testing"
	"time"
)

func TestAPIDocsEnabled(t *testing.T) {
	tests := []struct {
		name string
		env  string
		flag string
		want bool
	}{
		{name: "dev enabled", env: "dev", flag: "1", want: true},
		{name: "dev disabled", env: "dev", flag: "0", want: false},
		{name: "production enabled flag", env: "prod", flag: "1", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := apiDocsEnabled(test.env, func(string) string { return test.flag })
			if got != test.want {
				t.Fatalf("apiDocsEnabled(%q, %q) = %v, want %v", test.env, test.flag, got, test.want)
			}
		})
	}
}

func TestCrossInFlightSetting(t *testing.T) {
	t.Setenv("FARM_CROSS_MAX_IN_FLIGHT", "768")
	value, err := nonNegativeIntSetting("FARM_CROSS_MAX_IN_FLIGHT", 1024)
	if err != nil || value != 768 {
		t.Fatalf("nonNegativeIntSetting = %d, %v", value, err)
	}
	t.Setenv("FARM_CROSS_MAX_IN_FLIGHT", "-1")
	if _, err := nonNegativeIntSetting("FARM_CROSS_MAX_IN_FLIGHT", 1024); err == nil {
		t.Fatal("nonNegativeIntSetting accepted negative value")
	}
}

func TestWriteInFlightSetting(t *testing.T) {
	t.Setenv("FARM_WRITE_MAX_IN_FLIGHT", "640")
	value, err := nonNegativeIntSetting("FARM_WRITE_MAX_IN_FLIGHT", 512)
	if err != nil || value != 640 {
		t.Fatalf("nonNegativeIntSetting = %d, %v", value, err)
	}
}

func TestGatewayAdvertiseTargetUsesPodHostAndListenerPort(t *testing.T) {
	env := map[string]string{"FARM_GATEWAY_ADVERTISE_HOST": "10.42.0.7"}
	target, err := gatewayAdvertiseTarget(":9202", func(name string) string { return env[name] })
	if err != nil || target != "10.42.0.7:9202" {
		t.Fatalf("gatewayAdvertiseTarget = %q, %v", target, err)
	}
}

func TestGatewayAdvertiseTargetRejectsBadExplicitTarget(t *testing.T) {
	_, err := gatewayAdvertiseTarget(":9202", func(string) string { return "bad-target" })
	if err == nil {
		t.Fatal("gatewayAdvertiseTarget accepted invalid endpoint")
	}
}

func TestPositiveDurationSetting(t *testing.T) {
	t.Setenv("FARM_GATEWAY_INSTANCE_TTL", "45s")
	value, err := positiveDurationSetting("FARM_GATEWAY_INSTANCE_TTL", 30*time.Second)
	if err != nil || value != 45*time.Second {
		t.Fatalf("positiveDurationSetting = %s, %v", value, err)
	}
}
