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
		var envelope clientwire.Envelope
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return nil, err
		}
		if envelope.Cmd != CommandPushBatch {
			out = append(out, append(json.RawMessage(nil), frame...))
			continue
		}
		var payload struct {
			Envelopes []json.RawMessage `json:"envelopes"`
		}
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return nil, err
		}
		out = append(out, payload.Envelopes...)
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
		encoded, err := clientwire.EncodeFarmDelta(farm.FarmDelta{OwnerUID: 42, FarmSeq: uint64(i)})
		if err != nil {
			t.Fatalf("EncodeFarmDelta: %v", err)
		}
		raw = append(raw, encoded)
		if err := connection.pushFarmDelta(42, farm.FarmDelta{OwnerUID: 42, FarmSeq: uint64(i)}, encoded); err != nil {
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

	encoded, err := clientwire.EncodeFarmDelta(farm.FarmDelta{OwnerUID: 42, FarmSeq: 1})
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

	filler, err := clientwire.EncodeFarmDelta(farm.FarmDelta{OwnerUID: 1, FarmSeq: 1})
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
	encoded, err := clientwire.EncodeFarmDelta(farm.FarmDelta{OwnerUID: 42, FarmSeq: 9})
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
	if !bytes.Equal(messages[0], encoded) {
		t.Fatalf("single push should write raw envelope, got %s", messages[0])
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

	filler, err := clientwire.EncodeFarmDelta(farm.FarmDelta{OwnerUID: 1, FarmSeq: 1})
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
	encoded, err := clientwire.EncodeFarmDelta(farm.FarmDelta{OwnerUID: 42, FarmSeq: 1})
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
	var first clientwire.Envelope
	if err := json.Unmarshal(messages[0], &first); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if first.Cmd != CommandPing || first.ClientSeq != 7 {
		t.Fatalf("first write = %#v, want immediate Ping response", first)
	}
}

func TestClosePushWriterDoesNotLeakOrPanic(t *testing.T) {
	t.Parallel()

	connection, _ := newTestPushConn(t, time.Millisecond)
	encoded, err := clientwire.EncodeFarmDelta(farm.FarmDelta{OwnerUID: 42, FarmSeq: 1})
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
	var rsp clientwire.Envelope
	if err := json.Unmarshal(messages[0], &rsp); err != nil {
		t.Fatalf("unmarshal rsp: %v", err)
	}
	if rsp.Cmd != CommandEnterFarm || rsp.ClientSeq != 2 {
		t.Fatalf("first = %#v, want EnterFarm response", rsp)
	}
	delta, err := clientwire.DecodeFarmDelta(messages[1])
	if err != nil {
		t.Fatalf("second should be raw FarmDelta: %v (%s)", err, messages[1])
	}
	if delta.FarmSeq != 3 {
		t.Fatalf("held FarmSeq = %d", delta.FarmSeq)
	}
}

func TestMixedPushTypesPreserveOrderInBatch(t *testing.T) {
	t.Parallel()

	connection, writer := newTestPushConn(t, time.Millisecond)
	farmEnc, err := clientwire.EncodeFarmDelta(farm.FarmDelta{OwnerUID: 42, FarmSeq: 1})
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
	var batch clientwire.Envelope
	if err := json.Unmarshal(messages[0], &batch); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if batch.Cmd != CommandPushBatch {
		t.Fatalf("cmd = %d", batch.Cmd)
	}
	var payload struct {
		Envelopes []clientwire.Envelope `json:"envelopes"`
	}
	if err := json.Unmarshal(batch.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	want := []uint32{CommandFarmDelta, CommandPlayerDelta, CommandTaskNotify, CommandMailNotify}
	if len(payload.Envelopes) != len(want) {
		t.Fatalf("len = %d", len(payload.Envelopes))
	}
	for i, cmd := range want {
		if payload.Envelopes[i].Cmd != cmd || payload.Envelopes[i].ClientSeq != 0 {
			t.Fatalf("envelope[%d] = %#v", i, payload.Envelopes[i])
		}
	}
	if payload.Envelopes[0].Err != errcode.OK {
		t.Fatalf("farm err = %v", payload.Envelopes[0].Err)
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

	encoded, err := clientwire.EncodeFarmDelta(farm.FarmDelta{OwnerUID: 1, FarmSeq: 1})
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
