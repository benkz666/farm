package servicehost

import (
	"context"
	"errors"
	"testing"
	"time"

	"farm/server/shared/telemetry"

	"google.golang.org/grpc"
)

func TestWithGRPCReadyChecksBaseThenDependencyOnce(t *testing.T) {
	var baseCalls, readyCalls int
	base := telemetry.FuncChecker("storage", func(context.Context) error {
		baseCalls++
		return nil
	})
	checker := withGRPCReady(base, func(context.Context) error {
		readyCalls++
		return nil
	})

	if err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if baseCalls != 1 || readyCalls != 1 {
		t.Fatalf("calls = base:%d ready:%d, want 1/1", baseCalls, readyCalls)
	}
}

func TestStopGRPCWithTimeoutHandlesIdleServer(t *testing.T) {
	if err := stopGRPCWithTimeout(grpc.NewServer(), time.Second); err != nil {
		t.Fatalf("stopGRPCWithTimeout: %v", err)
	}
	if err := stopGRPCWithTimeout(nil, time.Second); err != nil {
		t.Fatalf("stopGRPCWithTimeout(nil): %v", err)
	}
}

func TestWithGRPCReadyStopsAfterBaseFailure(t *testing.T) {
	want := errors.New("storage unavailable")
	var readyCalls int
	base := telemetry.FuncChecker("storage", func(context.Context) error { return want })
	checker := withGRPCReady(base, func(context.Context) error {
		readyCalls++
		return nil
	})

	if err := checker.Check(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Check error = %v, want %v", err, want)
	}
	if readyCalls != 0 {
		t.Fatalf("ready calls = %d, want 0", readyCalls)
	}
}
