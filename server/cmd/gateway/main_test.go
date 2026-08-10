package main

import "testing"

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
