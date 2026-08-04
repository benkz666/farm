package gateway

import (
	"errors"
	"time"

	"farm/server/shared/clientwire"
)

const (
	// pushQueueCapacity bounds pending push frames per connection. Full queue
	// returns immediately so the caller can close the slow socket instead of
	// blocking farmsvr gRPC push.
	pushQueueCapacity = 64
	// pushCoalesceWindow gathers a short burst into one WebSocket WriteMessage.
	pushCoalesceWindow = time.Millisecond
	// pushBatchMax is the maximum number of push envelopes flushed in one write.
	// Matches clientwire.MaxPushBatchEnvelopes so a full channel drain stays legal.
	pushBatchMax = clientwire.MaxPushBatchEnvelopes
)

var (
	errPushClosed    = errors.New("gateway: push writer closed")
	errPushQueueFull = errors.New("gateway: push queue full")
)

// wsWireWriter is the gorilla WriteMessage seam used by tests to count physical
// frames without a real WebSocket.
type wsWireWriter interface {
	SetWriteDeadline(time.Time) error
	WriteMessage(messageType int, data []byte) error
	Close() error
}

func (connection *wsConnection) wire() wsWireWriter {
	if connection.writer != nil {
		return connection.writer
	}
	return connection.conn
}

// startPushWriter launches the per-connection coalesce loop. Safe to call once.
func (connection *wsConnection) startPushWriter() {
	if connection == nil {
		return
	}
	connection.pushMu.Lock()
	defer connection.pushMu.Unlock()
	if connection.pushStarted || connection.pushClosed {
		return
	}
	connection.pushStarted = true
	connection.pushCh = make(chan []byte, pushQueueCapacity)
	connection.pushStop = make(chan struct{})
	connection.pushDone = make(chan struct{})
	if connection.pushCoalesce == 0 {
		connection.pushCoalesce = pushCoalesceWindow
	}
	go connection.runPushWriter()
}

// closePushWriter stops the coalesce loop and waits for the writer to exit.
// It never closes pushCh, so enqueue cannot panic on send-on-closed.
// Safe after failPushWriter: pushClosed is already set and stop is closed.
func (connection *wsConnection) closePushWriter() {
	if connection == nil {
		return
	}
	connection.pushMu.Lock()
	started := connection.pushStarted
	done := connection.pushDone
	if !connection.pushClosed {
		connection.pushClosed = true
		if started {
			close(connection.pushStop)
		}
	}
	connection.pushMu.Unlock()
	if !started {
		return
	}
	<-done
}

// markPushClosed signals the writer to stop and makes enqueue return errPushClosed.
// It must not wait on pushDone — the writer goroutine itself calls this on write failure.
func (connection *wsConnection) markPushClosed() {
	if connection == nil {
		return
	}
	connection.pushMu.Lock()
	defer connection.pushMu.Unlock()
	if connection.pushClosed {
		return
	}
	connection.pushClosed = true
	if connection.pushStarted {
		close(connection.pushStop)
	}
}

// dropSlowConnection stops the push queue and asynchronously closes the wire.
// Callers on the gRPC/broadcast hot path must use this instead of a synchronous
// Close that could contend with a slow WriteMessage.
func (connection *wsConnection) dropSlowConnection() {
	if connection == nil {
		return
	}
	connection.markPushClosed()
	go func() {
		if writer := connection.wire(); writer != nil {
			_ = writer.Close()
		}
	}()
}

// failPushWriter terminates the coalesce loop after a write error. It marks the
// queue closed and closes the wire, but must not call closePushWriter (would
// deadlock waiting on pushDone from inside the writer).
func (connection *wsConnection) failPushWriter() {
	connection.markPushClosed()
	if writer := connection.wire(); writer != nil {
		_ = writer.Close()
	}
}

// enqueuePush schedules one already-encoded push Envelope for coalesced write.
// It is O(1) and never waits on WriteMessage / TLS.
func (connection *wsConnection) enqueuePush(encoded []byte) error {
	if connection == nil || len(encoded) == 0 {
		return errPushClosed
	}
	connection.pushMu.Lock()
	closed := connection.pushClosed
	started := connection.pushStarted
	ch := connection.pushCh
	stop := connection.pushStop
	connection.pushMu.Unlock()
	if closed || !started || ch == nil {
		return errPushClosed
	}
	select {
	case <-stop:
		return errPushClosed
	default:
	}
	select {
	case <-stop:
		return errPushClosed
	case ch <- encoded:
		return nil
	default:
		return errPushQueueFull
	}
}

func (connection *wsConnection) runPushWriter() {
	defer close(connection.pushDone)

	pending := make([][]byte, 0, pushBatchMax)
	var (
		timer  *time.Timer
		timerC <-chan time.Time
	)
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timerC:
			default:
			}
		}
		timer = nil
		timerC = nil
	}
	flush := func() bool {
		if len(pending) == 0 {
			return true
		}
		batch := pending
		pending = make([][]byte, 0, pushBatchMax)
		stopTimer()
		if err := connection.writePushBatch(batch); err != nil {
			connection.failPushWriter()
			return false
		}
		return true
	}

	for {
		select {
		case <-connection.pushStop:
			stopTimer()
			return
		case msg := <-connection.pushCh:
			pending = append(pending, msg)
			if len(pending) >= pushBatchMax {
				if !flush() {
					return
				}
				continue
			}
			if timer == nil {
				timer = time.NewTimer(connection.pushCoalesce)
				timerC = timer.C
			}
		case <-timerC:
			if !flush() {
				return
			}
		}
	}
}

// writePushBatch writes one physical WebSocket frame under writeMu.
// Strategy: a lone envelope is written raw (no 9010 wrapper); two or more are
// packed with EncodePushBatch so the client expands them in order.
func (connection *wsConnection) writePushBatch(envelopes [][]byte) error {
	if len(envelopes) == 0 {
		return nil
	}
	var (
		data []byte
		err  error
	)
	if len(envelopes) == 1 {
		data = envelopes[0]
	} else {
		data, err = clientwire.EncodePushBatch(envelopes)
		if err != nil {
			return err
		}
	}
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	return connection.writeEncodedLocked(data)
}
