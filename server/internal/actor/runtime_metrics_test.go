package actor_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"farm/server/internal/actor"
	"farm/server/internal/farm"
	"farm/server/internal/obs"
)

type memStore struct {
	agg *farm.Aggregate
}

func (m *memStore) LoadFarm(context.Context, uint64) (*farm.Aggregate, error) {
	if m.agg == nil {
		return farm.NewAggregate(1, "metrics"), nil
	}
	return m.agg, nil
}

func (m *memStore) SaveFarm(_ context.Context, agg *farm.Aggregate) error {
	m.agg = agg
	return nil
}

func TestRuntimeMetricsOmitUIDLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(reg)
	runtime := actor.NewRuntime(&memStore{}, time.Minute)
	runtime.SetMetrics(metrics)
	runtime.SetFlushInterval(time.Hour)

	if err := runtime.Do(7, func(a *actor.FarmActor) error {
		a.Aggregate.Coin += 1
		return nil
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = runtime.Shutdown(context.Background())

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var body strings.Builder
	for _, mf := range mfs {
		body.WriteString(mf.GetName())
		body.WriteByte('\n')
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				body.WriteString(lp.GetName() + "=" + lp.GetValue() + " ")
			}
		}
	}
	text := body.String()
	if !strings.Contains(text, "farm_actor_resident") {
		t.Fatalf("missing resident metric:\n%s", text)
	}
	if strings.Contains(text, "uid=") {
		t.Fatalf("actor metrics leaked uid label:\n%s", text)
	}
}
