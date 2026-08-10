package farmrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/grpcx"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GRPCClient implements Client over FarmCommandService.
type GRPCClient struct {
	pool    *grpcx.Pool
	targets map[string]string
	mu      sync.Mutex
	streams map[string]*commandStream
}

// NewGRPCClient constructs a routed Farm command client.
func NewGRPCClient(pool *grpcx.Pool, targets map[string]string) *GRPCClient {
	copied := make(map[string]string, len(targets))
	for farmID, target := range targets {
		copied[farmID] = target
	}
	return &GRPCClient{pool: pool, targets: copied, streams: make(map[string]*commandStream)}
}

// Execute forwards one command to the Farm instance that owns FarmUID.
func (client *GRPCClient) Execute(ctx context.Context, farmID string, command CommandRequest) (CommandResponse, error) {
	if client == nil || client.pool == nil {
		return CommandResponse{}, fmt.Errorf("farmrpc: gRPC client is nil")
	}
	target := client.targets[farmID]
	if target == "" {
		return CommandResponse{}, fmt.Errorf("farmrpc: no gRPC target configured for %q", farmID)
	}
	conn, err := client.pool.Conn(ctx, target)
	if err != nil {
		return CommandResponse{}, fmt.Errorf("farmrpc: dial %q: %w", farmID, err)
	}
	protoOperation, ok := operationToProtoEnum(command.Operation)
	if !ok {
		return CommandResponse{}, fmt.Errorf("farmrpc: unsupported operation %q", command.Operation)
	}
	request := &farmv1.ExecuteRequest{
		Operation:      protoOperation,
		FarmUid:        command.FarmUID,
		Originator:     connRefToProto(command.Originator),
		PayloadJson:    command.Payload,
		PreferPrepared: command.PreferPrepared,
	}
	if command.ClientCommand != 0 && command.ClientRequest != nil {
		request.PayloadJson = nil
		request.ClientCommand = &farmv1.ClientCommand{Command: command.ClientCommand, Request: command.ClientRequest}
	}
	commandStream, err := client.streamFor(target, conn)
	if err != nil {
		// Stream creation has not sent the command, so unary fallback is safe and
		// keeps rolling upgrades compatible with an older Farm server.
		response, unaryErr := farmv1.NewFarmCommandServiceClient(conn).Execute(ctx, request)
		if unaryErr != nil {
			return CommandResponse{}, fmt.Errorf("farmrpc: create stream: %v; unary execute: %w", err, unaryErr)
		}
		return commandResponseFromProto(response, command.ClientCommand), nil
	}
	response, err := commandStream.execute(ctx, request)
	if err != nil {
		// A caller deadline only abandons that caller's slot. The shared stream is
		// still healthy and may have thousands of unrelated in-flight commands;
		// tearing it down here would turn one timeout into a connection-wide outage.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return CommandResponse{}, fmt.Errorf("farmrpc: execute command: %w", err)
		}
		client.discardStream(target, commandStream)
		if status.Code(err) == codes.Unimplemented {
			response, unaryErr := farmv1.NewFarmCommandServiceClient(conn).Execute(ctx, request)
			if unaryErr == nil {
				return commandResponseFromProto(response, command.ClientCommand), nil
			}
			return CommandResponse{}, fmt.Errorf("farmrpc: stream unsupported: %v; unary execute: %w", err, unaryErr)
		}
		return CommandResponse{}, fmt.Errorf("farmrpc: execute command: %w", err)
	}
	return commandResponseFromProto(response, command.ClientCommand), nil
}

func commandResponseFromProto(response *farmv1.ExecuteResponse, command uint32) CommandResponse {
	if response == nil {
		return CommandResponse{Err: errcode.Internal}
	}
	result := CommandResponse{
		Err:             errcode.Code(response.Err),
		Payload:         json.RawMessage(response.PayloadJson),
		FarmSeq:         response.FarmSeq,
		PreparedPayload: response.PreparedPayload,
		PreparedField:   response.PreparedField,
		ClientResponse:  response.ClientResponse,
	}
	if result.ClientResponse != nil && !isHotActionCommand(command) {
		if payload, err := clientwire.CommandResponseToJSON(command, result.ClientResponse); err == nil {
			result.Payload = payload
		}
	}
	return result
}

func isHotActionCommand(command uint32) bool {
	return command >= 206 && command <= 220 && command%2 == 0 || command == 302 || command == 304
}

func (client *GRPCClient) streamFor(target string, conn grpc.ClientConnInterface) (*commandStream, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if current := client.streams[target]; current != nil && !current.failed() {
		return current, nil
	}
	stream, err := newCommandStream(farmv1.NewFarmCommandServiceClient(conn))
	if err != nil {
		return nil, err
	}
	client.streams[target] = stream
	return stream, nil
}

func (client *GRPCClient) discardStream(target string, stream *commandStream) {
	client.mu.Lock()
	if client.streams[target] == stream {
		delete(client.streams, target)
	}
	client.mu.Unlock()
	stream.stop(fmt.Errorf("farmrpc: stream discarded"))
}

type streamCall struct {
	ctx     context.Context
	id      uint64
	request *farmv1.ExecuteRequest
}

type streamResult struct {
	response *farmv1.ExecuteResponse
	err      error
}

const (
	commandStreamSlotBits       = 13
	commandStreamSlotCount      = 1 << commandStreamSlotBits
	commandStreamSlotMask       = commandStreamSlotCount - 1
	commandStreamSendQueue      = commandStreamSlotCount
	commandStreamBatchMax       = 64
	commandStreamCoalesceWindow = 150 * time.Microsecond

	streamSlotIdle uint32 = iota
	streamSlotWaiting
	streamSlotAbandoned
	streamSlotDelivered
)

type streamSlot struct {
	generation atomic.Uint64
	state      atomic.Uint32
	result     chan streamResult
}

// commandStream owns one batch sender and one batch receiver. A fixed slot
// table replaces the allocation-heavy per-request result channel and pending
// map; the generation embedded in request_id rejects late responses after reuse.
type commandStream struct {
	stream farmv1.FarmCommandService_ExecuteBatchStreamClient
	cancel context.CancelFunc
	send   chan streamCall
	free   chan uint32
	slots  []streamSlot
	done   chan struct{}
	once   sync.Once
	errMu  sync.Mutex
	err    error
}

func newCommandStream(client farmv1.FarmCommandServiceClient) (*commandStream, error) {
	streamCtx, cancel := context.WithCancel(context.Background())
	stream, err := client.ExecuteBatchStream(streamCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	commandStream := &commandStream{
		stream: stream,
		cancel: cancel,
		send:   make(chan streamCall, commandStreamSendQueue),
		free:   make(chan uint32, commandStreamSlotCount),
		slots:  make([]streamSlot, commandStreamSlotCount),
		done:   make(chan struct{}),
	}
	for index := range commandStream.slots {
		commandStream.slots[index].result = make(chan streamResult, 1)
		commandStream.free <- uint32(index)
	}
	go commandStream.sendLoop()
	go commandStream.receiveLoop()
	return commandStream, nil
}

func (stream *commandStream) execute(ctx context.Context, request *farmv1.ExecuteRequest) (*farmv1.ExecuteResponse, error) {
	var index uint32
	select {
	case index = <-stream.free:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-stream.done:
		return nil, stream.failure()
	}
	slot := &stream.slots[index]
	generation := slot.generation.Add(1)
	id := generation<<commandStreamSlotBits | uint64(index)
	slot.state.Store(streamSlotWaiting)
	call := streamCall{ctx: ctx, id: id, request: request}
	select {
	case stream.send <- call:
	case <-ctx.Done():
		slot.state.Store(streamSlotIdle)
		stream.free <- index
		return nil, ctx.Err()
	case <-stream.done:
		slot.state.Store(streamSlotIdle)
		stream.free <- index
		return nil, stream.failure()
	}
	select {
	case value := <-slot.result:
		stream.releaseDelivered(index, generation)
		return value.response, value.err
	case <-ctx.Done():
		if slot.state.CompareAndSwap(streamSlotWaiting, streamSlotAbandoned) {
			return nil, ctx.Err()
		}
		value := <-slot.result
		stream.releaseDelivered(index, generation)
		return value.response, value.err
	case <-stream.done:
		if slot.state.CompareAndSwap(streamSlotWaiting, streamSlotAbandoned) {
			return nil, stream.failure()
		}
		value := <-slot.result
		stream.releaseDelivered(index, generation)
		return value.response, value.err
	}
}

func (stream *commandStream) sendLoop() {
	for {
		select {
		case first := <-stream.send:
			calls := make([]streamCall, 0, commandStreamBatchMax)
			calls = append(calls, first)
			timer := time.NewTimer(commandStreamCoalesceWindow)
		collect:
			for len(calls) < commandStreamBatchMax {
				select {
				case call := <-stream.send:
					calls = append(calls, call)
				case <-timer.C:
					break collect
				case <-stream.done:
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					return
				}
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			requests := make([]*farmv1.StreamExecuteRequest, 0, len(calls))
			for _, call := range calls {
				if err := call.ctx.Err(); err != nil {
					stream.complete(call.id, streamResult{err: err})
					continue
				}
				requests = append(requests, &farmv1.StreamExecuteRequest{RequestId: call.id, Request: call.request})
			}
			if len(requests) == 0 {
				continue
			}
			if err := stream.stream.Send(&farmv1.StreamExecuteBatchRequest{Requests: requests}); err != nil {
				for _, request := range requests {
					stream.complete(request.RequestId, streamResult{err: err})
				}
				stream.stop(err)
				return
			}
		case <-stream.done:
			return
		}
	}
}

func (stream *commandStream) receiveLoop() {
	for {
		batch, err := stream.stream.Recv()
		if err != nil {
			if err == io.EOF {
				err = fmt.Errorf("farmrpc: command stream closed")
			}
			stream.stop(err)
			return
		}
		if batch == nil || len(batch.Responses) == 0 || len(batch.Responses) > commandStreamBatchMax {
			stream.stop(fmt.Errorf("farmrpc: malformed stream response"))
			return
		}
		for _, response := range batch.Responses {
			if response == nil || response.RequestId == 0 {
				stream.stop(fmt.Errorf("farmrpc: malformed stream response item"))
				return
			}
			stream.complete(response.RequestId, streamResult{response: response.Response})
		}
	}
}

func (stream *commandStream) complete(id uint64, result streamResult) {
	index := uint32(id & uint64(commandStreamSlotMask))
	if int(index) >= len(stream.slots) {
		return
	}
	slot := &stream.slots[index]
	if slot.generation.Load() != id>>commandStreamSlotBits {
		return
	}
	if slot.state.CompareAndSwap(streamSlotWaiting, streamSlotDelivered) {
		slot.result <- result
		return
	}
	if slot.state.CompareAndSwap(streamSlotAbandoned, streamSlotIdle) {
		stream.free <- index
	}
}

func (stream *commandStream) releaseDelivered(index uint32, generation uint64) {
	slot := &stream.slots[index]
	if slot.generation.Load() != generation || !slot.state.CompareAndSwap(streamSlotDelivered, streamSlotIdle) {
		return
	}
	stream.free <- index
}

func (stream *commandStream) stop(err error) {
	stream.once.Do(func() {
		stream.errMu.Lock()
		stream.err = err
		stream.errMu.Unlock()
		stream.cancel()
		close(stream.done)
		for index := range stream.slots {
			slot := &stream.slots[index]
			if slot.state.CompareAndSwap(streamSlotWaiting, streamSlotDelivered) {
				slot.result <- streamResult{err: err}
			} else if slot.state.CompareAndSwap(streamSlotAbandoned, streamSlotIdle) {
				stream.free <- uint32(index)
			}
		}
	})
}

func (stream *commandStream) failed() bool {
	select {
	case <-stream.done:
		return true
	default:
		return false
	}
}

func (stream *commandStream) failure() error {
	stream.errMu.Lock()
	defer stream.errMu.Unlock()
	if stream.err == nil {
		return fmt.Errorf("farmrpc: command stream unavailable")
	}
	return stream.err
}

// RegisterCommandService registers FarmCommandService on a gRPC server.
func RegisterCommandService(server *grpc.Server, handler *Handler, owns func(uint64) bool) {
	farmv1.RegisterFarmCommandServiceServer(server, NewCommandServer(handler, owns))
}
