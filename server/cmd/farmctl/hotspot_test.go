package main

import (
	"strings"
	"testing"
	"time"
)

func TestHotspotLocalOnly(t *testing.T) {
	if !hotspotLocalOnly(
		"http://127.0.0.1:9002",
		"localhost:9210",
		"farm:farm@tcp(127.0.0.1:3306)/farm",
	) {
		t.Fatal("loopback endpoints should be accepted")
	}
	if hotspotLocalOnly(
		"https://farm.example.com",
		"farm.example.com:9210",
		"farm:farm@tcp(db.example.com:3306)/farm",
	) {
		t.Fatal("remote endpoints must be rejected")
	}
}

func TestRenderHotspot(t *testing.T) {
	lines := renderHotspot([]hotspotOutcome{
		{latency: 10 * time.Millisecond, code: 0},
		{latency: 20 * time.Millisecond, code: 1211},
		{latency: 30 * time.Millisecond, rpcErr: "DeadlineExceeded", ackErr: "Internal"},
	}, 100*time.Millisecond)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"样本=3",
		"RPC错误=1",
		"ACK错误=1",
		"吞吐=30.0 req/s",
		"0=1",
		"1211=1",
		"grpc:DeadlineExceeded=1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("rendered output missing %q:\n%s", want, joined)
		}
	}
}
