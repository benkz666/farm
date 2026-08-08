package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/store"
)

type countingWireWriter struct {
	mu       sync.Mutex
	writes   [][]byte
	writeN   atomic.Int64
	closeN   atomic.Int64
	deadline time.Time
}

func (w *countingWireWriter) SetWriteDeadline(t time.Time) error {
	w.deadline = t
	return nil
}

func (w *countingWireWriter) WriteMessage(_ int, data []byte) error {
	w.writeN.Add(1)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, append([]byte(nil), data...))
	return nil
}

func (w *countingWireWriter) Close() error {
	w.closeN.Add(1)
	return nil
}

func (w *countingWireWriter) messages() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([][]byte, len(w.writes))
	for i, msg := range w.writes {
		out[i] = append([]byte(nil), msg...)
	}
	return out
}

type failingWireWriter struct {
	countingWireWriter
	failOn int64
}

func (w *failingWireWriter) WriteMessage(messageType int, data []byte) error {
	n := w.writeN.Add(1)
	if n >= w.failOn {
		return errors.New("write boom")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, append([]byte(nil), data...))
	return nil
}

func expandPhysicalPushFrames(frames [][]byte) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0)
	for _, frame := range frames {
		envelopes, err := clientwire.DecodeBinaryBatch(frame)
		if err != nil {
			return nil, err
		}
		for _, envelope := range envelopes {
			encoded, err := clientwire.EncodeEnvelope(envelope)
			if err != nil {
				return nil, err
			}
			out = append(out, encoded)
		}
	}
	return out, nil
}

func newTestPushConn(t *testing.T, coalesce time.Duration) (*wsConnection, *countingWireWriter) {
	t.Helper()
	writer := &countingWireWriter{}
	connection := &wsConnection{
		writer:       writer,
		pushCoalesce: coalesce,
		roomUID:      42,
	}
	connection.startPushWriter()
	t.Cleanup(connection.closePushWriter)
	return connection, writer
}

func waitWrites(t *testing.T, writer *countingWireWriter, want int, timeout time.Duration) [][]byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if int(writer.writeN.Load()) >= want {
			return writer.messages()
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d writes, got %d", want, writer.writeN.Load())
	return nil
}

func TestPushCoalesceBatchesMultipleWrites(t *testing.T) {
	t.Parallel()

	connection, writer := newTestPushConn(t, time.Millisecond)
	const n = 32
	raw := make([][]byte, 0, n)
	for i := 1; i <= n; i++ {
		delta := farm.FarmDelta{OwnerUID: 42, FarmSeq: uint64(i)}
		expected, err := clientwire.EncodeFarmDelta(delta)
		if err != nil {
			t.Fatalf("EncodeFarmDelta: %v", err)
		}
		encoded, err := clientwire.EncodeFarmDeltaRecord(delta)
		if err != nil {
			t.Fatalf("EncodeFarmDeltaRecord: %v", err)
		}
		raw = append(raw, expected)
		if err := connection.pushFarmDelta(42, delta, encoded); err != nil {
			t.Fatalf("pushFarmDelta %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(time.Second)
	var (
		all    []json.RawMessage
		err    error
		writes int64
	)
	for time.Now().Before(deadline) {
		messages := writer.messages()
		all, err = expandPhysicalPushFrames(messages)
		if err != nil {
			t.Fatalf("expand frames: %v", err)
		}
		writes = writer.writeN.Load()
		if len(all) >= n {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(all) != n {
		t.Fatalf("expanded envelopes = %d, want %d across %d physical writes", len(all), n, writes)
	}
	// -race 和繁忙调度器可能让 1ms timer 在生产循环中多次触发；这里只验证
	// 合并确实显著减少物理写，不把测试绑定到精确的调度次数。
	if writes > int64(n/2) {
		t.Fatalf("WriteMessage count = %d, want <= %d", writes, n/2)
	}
	for i, envelope := range all {
		if !bytes.Equal(envelope, raw[i]) {
			t.Fatalf("envelope %d mutated or reordered", i)
		}
		delta, err := clientwire.DecodeFarmDelta(envelope)
		if err != nil {
			t.Fatalf("DecodeFarmDelta %d: %v", i, err)
		}
		if delta.FarmSeq != uint64(i+1) {
			t.Fatalf("FarmSeq[%d] = %d", i, delta.FarmSeq)
		}
	}
}

func TestPushWriterWriteFailureClosesAndStopsEnqueue(t *testing.T) {
	t.Parallel()

	writer := &failingWireWriter{failOn: 1}
	connection := &wsConnection{
		writer:       writer,
		pushCoalesce: time.Millisecond,
		roomUID:      42,
	}
	connection.startPushWriter()
	t.Cleanup(connection.closePushWriter)

	encoded, err := clientwire.EncodeFarmDeltaRecord(farm.FarmDelta{OwnerUID: 42, FarmSeq: 1})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	if err := connection.enqueuePush(encoded); err != nil {
		t.Fatalf("enqueuePush: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if writer.closeN.Load() > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if writer.closeN.Load() == 0 {
		t.Fatal("write failure did not Close the wire")
	}

	deadline = time.Now().Add(time.Second)
	var enqueueErr error
	for time.Now().Before(deadline) {
		enqueueErr = connection.enqueuePush(encoded)
		if errors.Is(enqueueErr, errPushClosed) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(enqueueErr, errPushClosed) {
		t.Fatalf("enqueue after write failure = %v, want %v", enqueueErr, errPushClosed)
	}
}

func TestDropSlowConnectionOnQueueFull(t *testing.T) {
	t.Parallel()

	writer := &countingWireWriter{}
	connection := &wsConnection{writer: writer}
	connection.pushMu.Lock()
	connection.pushStarted = true
	connection.pushCh = make(chan []byte, pushQueueCapacity)
	connection.pushStop = make(chan struct{})
	connection.pushDone = make(chan struct{})
	close(connection.pushDone) // no live writer; closePushWriter/mark must stay safe
	connection.pushMu.Unlock()

	filler, err := clientwire.EncodeFarmDeltaRecord(farm.FarmDelta{OwnerUID: 1, FarmSeq: 1})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	for i := 0; i < pushQueueCapacity; i++ {
		if err := connection.enqueuePush(filler); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := connection.enqueuePush(filler); !errors.Is(err, errPushQueueFull) {
		t.Fatalf("overflow = %v, want %v", err, errPushQueueFull)
	}
	connection.dropSlowConnection()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if writer.closeN.Load() > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if writer.closeN.Load() == 0 {
		t.Fatal("dropSlowConnection did not Close the wire")
	}
	if err := connection.enqueuePush(filler); !errors.Is(err, errPushClosed) {
		t.Fatalf("enqueue after drop = %v, want %v", err, errPushClosed)
	}
}

func TestPushCoalesceSingleWriteRaw(t *testing.T) {
	t.Parallel()

	connection, writer := newTestPushConn(t, time.Millisecond)
	encoded, err := clientwire.EncodeFarmDeltaRecord(farm.FarmDelta{OwnerUID: 42, FarmSeq: 9})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	if err := connection.enqueuePush(encoded); err != nil {
		t.Fatalf("enqueuePush: %v", err)
	}
	messages := waitWrites(t, writer, 1, time.Second)
	if writer.writeN.Load() != 1 {
		t.Fatalf("writes = %d", writer.writeN.Load())
	}
	batch, err := clientwire.DecodeBinaryBatch(messages[0])
	if err != nil || len(batch) != 1 || batch[0].Cmd != CommandFarmDelta {
		t.Fatalf("single push binary batch = %#v err=%v", batch, err)
	}
}

func TestPushQueueFullReturnsError(t *testing.T) {
	t.Parallel()

	connection := &wsConnection{}
	connection.pushMu.Lock()
	connection.pushStarted = true
	connection.pushCh = make(chan []byte, pushQueueCapacity)
	connection.pushStop = make(chan struct{})
	connection.pushMu.Unlock()

	filler, err := clientwire.EncodeFarmDeltaRecord(farm.FarmDelta{OwnerUID: 1, FarmSeq: 1})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	for i := 0; i < pushQueueCapacity; i++ {
		if err := connection.enqueuePush(filler); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := connection.enqueuePush(filler); err != errPushQueueFull {
		t.Fatalf("enqueue overflow = %v, want %v", err, errPushQueueFull)
	}
}

func TestRespondDoesNotWaitCoalesceWindow(t *testing.T) {
	t.Parallel()

	connection, writer := newTestPushConn(t, 50*time.Millisecond)
	encoded, err := clientwire.EncodeFarmDeltaRecord(farm.FarmDelta{OwnerUID: 42, FarmSeq: 1})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	if err := connection.enqueuePush(encoded); err != nil {
		t.Fatalf("enqueuePush: %v", err)
	}

	start := time.Now()
	if err := connection.respond(Envelope{
		Cmd:       CommandPing,
		ClientSeq: 7,
		Payload:   emptyPayload,
	}); err != nil {
		t.Fatalf("respond: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Fatalf("respond waited %v; must not wait for coalesce window", elapsed)
	}

	messages := waitWrites(t, writer, 1, time.Second)
	firstBatch, err := clientwire.DecodeBinaryBatch(messages[0])
	if err != nil || len(firstBatch) != 1 {
		t.Fatalf("decode first batch: len=%d err=%v", len(firstBatch), err)
	}
	first := firstBatch[0]
	if first.Cmd != CommandPing || first.ClientSeq != 7 {
		t.Fatalf("first write = %#v, want immediate Ping response", first)
	}
}

func TestClosePushWriterDoesNotLeakOrPanic(t *testing.T) {
	t.Parallel()

	connection, _ := newTestPushConn(t, time.Millisecond)
	encoded, err := clientwire.EncodeFarmDeltaRecord(farm.FarmDelta{OwnerUID: 42, FarmSeq: 1})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	_ = connection.enqueuePush(encoded)
	connection.closePushWriter()
	connection.closePushWriter() // idempotent
	if err := connection.enqueuePush(encoded); err != errPushClosed {
		t.Fatalf("enqueue after close = %v, want %v", err, errPushClosed)
	}
}

func TestEnterFarmHeldDeltasEnqueueAfterResponse(t *testing.T) {
	t.Parallel()

	connection, writer := newTestPushConn(t, time.Millisecond)
	connection.roomUID = 42
	connection.holdFarmDeltas = true
	if err := connection.pushFarmDelta(42, farm.FarmDelta{OwnerUID: 42, FarmSeq: 3}, nil); err != nil {
		t.Fatalf("held push: %v", err)
	}
	if err := connection.respondEnterFarm(Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"farm_seq":"2","snapshot":{},"server_time":1,"relation":"SELF"}`),
	}); err != nil {
		t.Fatalf("respondEnterFarm: %v", err)
	}

	messages := waitWrites(t, writer, 2, time.Second)
	rspBatch, err := clientwire.DecodeBinaryBatch(messages[0])
	if err != nil || len(rspBatch) != 1 {
		t.Fatalf("decode rsp batch: len=%d err=%v", len(rspBatch), err)
	}
	rsp := rspBatch[0]
	if rsp.Cmd != CommandEnterFarm || rsp.ClientSeq != 2 {
		t.Fatalf("first = %#v, want EnterFarm response", rsp)
	}
	deltaBatch, err := clientwire.DecodeBinaryBatch(messages[1])
	if err != nil || len(deltaBatch) != 1 {
		t.Fatalf("second should be binary FarmDelta: len=%d err=%v", len(deltaBatch), err)
	}
	var delta farm.FarmDelta
	if err := json.Unmarshal(deltaBatch[0].Payload, &delta); err != nil {
		t.Fatalf("decode FarmDelta payload: %v", err)
	}
	if delta.FarmSeq != 3 {
		t.Fatalf("held FarmSeq = %d", delta.FarmSeq)
	}
}

func TestMixedPushTypesPreserveOrderInBatch(t *testing.T) {
	t.Parallel()

	// Race instrumentation can stretch four sequential encodes beyond the
	// production 1 ms window and split this focused ordering assertion into two
	// legal frames. Use a wider test-only window so the intended single-batch
	// precondition is deterministic.
	connection, writer := newTestPushConn(t, 50*time.Millisecond)
	farmEnc, err := clientwire.EncodeFarmDeltaRecord(farm.FarmDelta{OwnerUID: 42, FarmSeq: 1})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	if err := connection.enqueuePush(farmEnc); err != nil {
		t.Fatalf("farm: %v", err)
	}
	if err := connection.pushPlayerDelta(farm.PlayerDelta{Coin: 10}); err != nil {
		t.Fatalf("player: %v", err)
	}
	if err := connection.pushTaskNotify(store.Task{ID: store.TaskPlantID, Progress: 1, Target: 1}); err != nil {
		t.Fatalf("task: %v", err)
	}
	if err := connection.pushMailNotify("new_mail"); err != nil {
		t.Fatalf("mail: %v", err)
	}

	messages := waitWrites(t, writer, 1, time.Second)
	envelopes, err := clientwire.DecodeBinaryBatch(messages[0])
	if err != nil {
		t.Fatalf("decode binary batch: %v", err)
	}
	want := []uint32{CommandFarmDelta, CommandPlayerDelta, CommandTaskNotify, CommandMailNotify}
	if len(envelopes) != len(want) {
		t.Fatalf("len = %d", len(envelopes))
	}
	for i, cmd := range want {
		if envelopes[i].Cmd != cmd || envelopes[i].ClientSeq != 0 {
			t.Fatalf("envelope[%d] = %#v", i, envelopes[i])
		}
	}
	if envelopes[0].Err != errcode.OK {
		t.Fatalf("farm err = %v", envelopes[0].Err)
	}
}

func BenchmarkPushCoalesce32(b *testing.B) {
	writer := &countingWireWriter{}
	connection := &wsConnection{
		writer:       writer,
		pushCoalesce: time.Millisecond,
		roomUID:      1,
	}
	connection.startPushWriter()
	defer connection.closePushWriter()

	encoded, err := clientwire.EncodeFarmDeltaRecord(farm.FarmDelta{OwnerUID: 1, FarmSeq: 1})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		writer.writeN.Store(0)
		writer.mu.Lock()
		writer.writes = writer.writes[:0]
		writer.mu.Unlock()
		for j := 0; j < 32; j++ {
			if err := connection.enqueuePush(encoded); err != nil {
				b.Fatal(err)
			}
		}
		deadline := time.Now().Add(time.Second)
		for writer.writeN.Load() < 1 {
			if time.Now().After(deadline) {
				b.Fatal("timeout")
			}
			time.Sleep(100 * time.Microsecond)
		}
	}
}
