package farmrpc

import (
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"farm/server/domain/farm"
	"farm/server/gateway/presence"
	"farm/server/shared/clientwire"
	"farm/server/shared/telemetry"
)

func TestFanoutPublisherBatchesSameGatewayOnce(t *testing.T) {
	t.Parallel()

	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend)
	for _, connID := range []uint64{1, 2, 3} {
		if err := registry.Subscribe(t.Context(), 42, connID, "gateway-a"); err != nil {
			t.Fatalf("Subscribe %d: %v", connID, err)
		}
	}
	pusher := &recordingBatchPusher{}
	publisher := NewFanoutPublisher(registry, pusher)
	delta := farm.FarmDelta{OwnerUID: 42, FarmSeq: 9}

	if err := publisher.Publish(t.Context(), delta, presence.ConnRef{}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	batches := pusher.batches()
	if len(batches) != 1 {
		t.Fatalf("batches = %d, want 1 HTTP push for same Gateway", len(batches))
	}
	if batches[0].gatewayID != "gateway-a" {
		t.Fatalf("gateway = %q", batches[0].gatewayID)
	}
	gotIDs := append([]uint64(nil), batches[0].batch.ConnIDs...)
	sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
	if !equalUint64(gotIDs, []uint64{1, 2, 3}) {
		t.Fatalf("conn_ids = %#v, want [1 2 3]", gotIDs)
	}
	if got := clientwire.FarmDeltaFromProto(batches[0].batch.Delta); !reflect.DeepEqual(got, delta) {
		t.Fatalf("typed FarmDelta = %#v, want %#v", got, delta)
	}
}

func TestFanoutPublisherBatchesAcrossTwoGateways(t *testing.T) {
	t.Parallel()

	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend)
	if err := registry.Subscribe(t.Context(), 42, 1, "gateway-a"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 2, "gateway-a"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 3, "gateway-b"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	pusher := &recordingBatchPusher{}
	publisher := NewFanoutPublisher(registry, pusher)

	if err := publisher.Publish(t.Context(), farm.FarmDelta{OwnerUID: 42, FarmSeq: 1}, presence.ConnRef{}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	batches := pusher.batches()
	if len(batches) != 2 {
		t.Fatalf("batches = %d, want 2 (one per Gateway)", len(batches))
	}
	byGateway := map[string][]uint64{}
	for _, item := range batches {
		ids := append([]uint64(nil), item.batch.ConnIDs...)
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		byGateway[item.gatewayID] = ids
	}
	if !equalUint64(byGateway["gateway-a"], []uint64{1, 2}) {
		t.Fatalf("gateway-a conn_ids = %#v", byGateway["gateway-a"])
	}
	if !equalUint64(byGateway["gateway-b"], []uint64{3}) {
		t.Fatalf("gateway-b conn_ids = %#v", byGateway["gateway-b"])
	}
}

func TestFanoutPublisherExcludesOriginatorFromBatch(t *testing.T) {
	t.Parallel()

	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend)
	if err := registry.Subscribe(t.Context(), 42, 1, "gateway-a"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 2, "gateway-a"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 3, "gateway-b"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	pusher := &recordingBatchPusher{}
	publisher := NewFanoutPublisher(registry, pusher)

	if err := publisher.Publish(t.Context(), farm.FarmDelta{OwnerUID: 42, FarmSeq: 1},
		presence.ConnRef{ConnID: 2, GatewayID: "gateway-a"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	batches := pusher.batches()
	byGateway := map[string][]uint64{}
	for _, item := range batches {
		ids := append([]uint64(nil), item.batch.ConnIDs...)
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		byGateway[item.gatewayID] = ids
	}
	if !equalUint64(byGateway["gateway-a"], []uint64{1}) {
		t.Fatalf("gateway-a conn_ids = %#v, want originator 2 excluded", byGateway["gateway-a"])
	}
	if !equalUint64(byGateway["gateway-b"], []uint64{3}) {
		t.Fatalf("gateway-b conn_ids = %#v", byGateway["gateway-b"])
	}
}

func TestFanoutPublisherResolvesQueuedDeltasInOneBatch(t *testing.T) {
	t.Parallel()

	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend)
	const count = 128
	jobs := make([]queuedDelta, 0, count)
	for uid := uint64(1); uid <= count; uid++ {
		origin := presence.ConnRef{ConnID: uid, GatewayID: "gateway-a"}
		if err := registry.Subscribe(t.Context(), uid, origin.ConnID, origin.GatewayID); err != nil {
			t.Fatalf("Subscribe %d: %v", uid, err)
		}
		jobs = append(jobs, queuedDelta{
			delta:      farm.FarmDelta{OwnerUID: uid, FarmSeq: 1},
			originator: origin,
		})
	}
	pusher := &recordingBatchPusher{}
	publisher := NewFanoutPublisher(registry, pusher)
	if err := publisher.PublishBatch(t.Context(), jobs); err != nil {
		t.Fatalf("PublishBatch: %v", err)
	}
	if calls := backend.batchCallCount(); calls != 1 {
		t.Fatalf("subscriber batch calls = %d, want 1", calls)
	}
	if batches := pusher.batches(); len(batches) != 0 {
		t.Fatalf("originator-only jobs produced pushes: %#v", batches)
	}
}

func TestFanoutPublisherMetricsCountsBatchesPerGateway(t *testing.T) {
	t.Parallel()

	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend)
	// 3 连接 / 2 Gateway：gateway-a 两条、gateway-b 一条 → 2 batch、3 targets、encode 1 次。
	if err := registry.Subscribe(t.Context(), 42, 1, "gateway-a"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 2, "gateway-a"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 3, "gateway-b"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	reg := prometheus.NewRegistry()
	metrics := telemetry.NewMetrics(reg)
	encoder := &countingFarmDeltaEncoder{}
	pusher := &recordingBatchPusher{}
	publisher := NewFanoutPublisher(registry, pusher)
	publisher.encoder = encoder
	publisher.SetMetrics(metrics)

	if err := publisher.Publish(t.Context(), farm.FarmDelta{OwnerUID: 42, FarmSeq: 1}, presence.ConnRef{}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := encoder.calls.Load(); got != 1 {
		t.Fatalf("encode calls = %d, want 1", got)
	}
	if got := len(pusher.batches()); got != 2 {
		t.Fatalf("HTTP batches = %d, want 2", got)
	}
	if got := testutil.ToFloat64(metrics.DeltaBatches); got != 2 {
		t.Fatalf("farm_delta_broadcast_batches_total = %v, want 2", got)
	}
	count, sum := histogramCountSum(t, reg, "farm_delta_broadcast_targets")
	if count != 1 || sum != 3 {
		t.Fatalf("targets histogram count=%v sum=%v, want count=1 sum=3", count, sum)
	}

	// 无订阅目标时不得记虚假 batch。
	empty := NewFanoutPublisher(presence.NewWithBackend(newRegistryBackend()), &recordingBatchPusher{})
	empty.SetMetrics(metrics)
	if err := empty.Publish(t.Context(), farm.FarmDelta{OwnerUID: 99, FarmSeq: 1}, presence.ConnRef{}); err != nil {
		t.Fatalf("empty Publish: %v", err)
	}
	if got := testutil.ToFloat64(metrics.DeltaBatches); got != 2 {
		t.Fatalf("empty publish mutated batches to %v, want 2", got)
	}
}

func histogramCountSum(t *testing.T, g prometheus.Gatherer, name string) (count, sum float64) {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			h := m.GetHistogram()
			if h == nil {
				continue
			}
			return float64(h.GetSampleCount()), h.GetSampleSum()
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0, 0
}

func TestFanoutPublisherEncodesEnvelopeOnce(t *testing.T) {
	t.Parallel()

	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend)
	if err := registry.Subscribe(t.Context(), 42, 1, "gateway-a"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 2, "gateway-a"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 3, "gateway-b"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	encoder := &countingFarmDeltaEncoder{}
	pusher := &recordingBatchPusher{}
	publisher := NewFanoutPublisher(registry, pusher)
	publisher.encoder = encoder

	if err := publisher.Publish(t.Context(), farm.FarmDelta{OwnerUID: 42, FarmSeq: 4}, presence.ConnRef{}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := encoder.calls.Load(); got != 1 {
		t.Fatalf("encode calls = %d, want 1 (not once per Gateway)", got)
	}
	batches := pusher.batches()
	if len(batches) != 2 {
		t.Fatalf("batches = %d, want 2", len(batches))
	}
	if string(batches[0].batch.Envelope) != string(batches[1].batch.Envelope) {
		t.Fatalf("gateways received different envelope bytes")
	}
}

func TestFanoutPublisherSlowGatewayDoesNotBlockOthers(t *testing.T) {
	t.Parallel()

	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend)
	if err := registry.Subscribe(t.Context(), 42, 1, "slow"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 2, "fast"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	fastStarted := make(chan struct{})
	fastDone := make(chan struct{})
	slowHold := make(chan struct{})
	pusher := &gatedBatchPusher{
		slowGateway: "slow",
		onFast: func() {
			close(fastStarted)
			close(fastDone)
		},
		holdSlow: slowHold,
	}
	publisher := NewFanoutPublisher(registry, pusher)

	errCh := make(chan error, 1)
	go func() {
		errCh <- publisher.Publish(t.Context(), farm.FarmDelta{OwnerUID: 42, FarmSeq: 1}, presence.ConnRef{})
	}()

	select {
	case <-fastStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("fast Gateway was blocked behind slow Gateway")
	}
	select {
	case <-fastDone:
	case <-time.After(time.Second):
		t.Fatal("fast Gateway push did not finish")
	}
	close(slowHold)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Publish did not return after slow Gateway released")
	}
}

func TestFanoutPublisherFailedGatewayDoesNotPreventOthers(t *testing.T) {
	t.Parallel()

	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend)
	if err := registry.Subscribe(t.Context(), 42, 1, "bad"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 2, "good"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	pusher := &failingBatchPusher{failGateway: "bad"}
	publisher := NewFanoutPublisher(registry, pusher)

	err := publisher.Publish(t.Context(), farm.FarmDelta{OwnerUID: 42, FarmSeq: 1}, presence.ConnRef{})
	if err == nil {
		t.Fatal("Publish error = nil, want failed Gateway error")
	}
	if !pusher.saw("good") {
		t.Fatal("good Gateway was not attempted after bad Gateway failure")
	}
	if !pusher.saw("bad") {
		t.Fatal("bad Gateway was not attempted")
	}
}

func TestFanoutPublisherPublishRace(t *testing.T) {
	t.Parallel()

	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend)
	for i := uint64(1); i <= 8; i++ {
		gatewayID := "gateway-a"
		if i%2 == 0 {
			gatewayID = "gateway-b"
		}
		if err := registry.Subscribe(t.Context(), 42, i, gatewayID); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
	}
	pusher := &recordingBatchPusher{}
	publisher := NewFanoutPublisher(registry, pusher)

	var wg sync.WaitGroup
	for seq := uint64(1); seq <= 20; seq++ {
		wg.Add(1)
		go func(seq uint64) {
			defer wg.Done()
			_ = publisher.Publish(t.Context(), farm.FarmDelta{OwnerUID: 42, FarmSeq: seq}, presence.ConnRef{})
		}(seq)
	}
	wg.Wait()
	if got := len(pusher.batches()); got < 40 {
		t.Fatalf("batches = %d, want at least 40 (20 publishes × 2 gateways)", got)
	}
}
