package farmrpc

import (
	"context"
	"errors"
	"io"
	"time"

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/presence"
	"farm/server/shared/telemetry"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	streamBatchMax               = 64
	streamResponseCoalesceWindow = 50 * time.Microsecond
)

// ClientExecutor owns command validation and all Farm business behavior.
type ClientExecutor interface {
	ExecuteClient(context.Context, *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse
}

type preparedSelfSyncExecutor interface {
	ExecutePreparedSelfSync(context.Context, uint64, presence.ConnRef, uint64) CommandResponse
}

// TaskAdvancer is implemented by Farm's application handler for cross-shard
// task side effects. It is intentionally not part of Gateway's command path.
type TaskAdvancer interface {
	AdvanceTask(context.Context, uint64, uint32, uint32) errcode.Code
}

// CommandServer is the typed gRPC boundary used by Gateway.
type CommandServer struct {
	farmv1.UnimplementedFarmCommandServiceServer
	executor ClientExecutor
	fastSync preparedSelfSyncExecutor
	tasks    TaskAdvancer
	owns     func(uint64) bool
	metrics  *telemetry.Metrics
}

func NewCommandServer(
	executor ClientExecutor,
	owns func(uint64) bool,
	metrics ...*telemetry.Metrics,
) *CommandServer {
	tasks, _ := executor.(TaskAdvancer)
	fastSync, _ := executor.(preparedSelfSyncExecutor)
	server := &CommandServer{executor: executor, fastSync: fastSync, tasks: tasks, owns: owns}
	if len(metrics) != 0 {
		server.metrics = metrics[0]
	}
	return server
}

func (server *CommandServer) Execute(ctx context.Context, request *farmv1.ClientCommandRequest) (*farmv1.ClientCommandResponse, error) {
	return server.execute(ctx, request), nil
}

func (server *CommandServer) AdvanceTask(
	ctx context.Context,
	request *farmv1.AdvanceTaskRequest,
) (*farmv1.AdvanceTaskResponse, error) {
	if server == nil || server.tasks == nil || request == nil || request.Uid == 0 ||
		request.TaskId == 0 || request.Amount == 0 || server.owns != nil && !server.owns(request.Uid) {
		return &farmv1.AdvanceTaskResponse{Err: int32(errcode.BadRequest)}, nil
	}
	return &farmv1.AdvanceTaskResponse{
		Err: int32(server.tasks.AdvanceTask(ctx, request.Uid, request.TaskId, request.Amount)),
	}, nil
}

func (server *CommandServer) execute(ctx context.Context, request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
	if server == nil || server.executor == nil || request == nil || request.Uid == 0 ||
		request.RouteUid == 0 || request.Envelope == nil ||
		server.owns != nil && !server.owns(request.RouteUid) {
		return errorClientResponse(request, errcode.BadRequest)
	}
	return server.executor.ExecuteClient(ctx, request)
}

func errorClientResponse(request *farmv1.ClientCommandRequest, code errcode.Code) *farmv1.ClientCommandResponse {
	var command, sequence uint32
	if request != nil && request.Envelope != nil {
		command, sequence = request.Envelope.Cmd, request.Envelope.ClientSeq
	}
	return &farmv1.ClientCommandResponse{Envelope: errorEnvelope(command, sequence, code)}
}

func errorEnvelope(command, sequence uint32, code errcode.Code) *publicv3.WireEnvelope {
	return &publicv3.WireEnvelope{
		Cmd: command, ClientSeq: sequence, Err: int32(code),
		Payload: &publicv3.WireEnvelope_CommandResponse{CommandResponse: &publicv3.CommandResponse{}},
	}
}

func (server *CommandServer) ExecuteStream(stream farmv1.FarmCommandService_ExecuteStreamServer) error {
	for {
		request, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		response := &farmv1.StreamExecuteResponse{RequestId: request.GetRequestId()}
		if request == nil || request.RequestId == 0 || request.Request == nil {
			response.Response = errorClientResponse(request.GetRequest(), errcode.BadRequest)
		} else {
			response.Response = server.execute(stream.Context(), request.Request)
		}
		if err := stream.Send(response); err != nil {
			return err
		}
	}
}

func (server *CommandServer) ExecuteBatchStream(stream farmv1.FarmCommandService_ExecuteBatchStreamServer) error {
	sizing := currentStreamSchedulerSizing()
	completed := make(chan *farmv1.StreamExecuteResponse, (sizing.normalConcurrency+sizing.barrierConcurrency)*2)
	done := make(chan struct{})
	scheduler := newStreamRequestSchedulerWithSizing(stream.Context(), server, completed, done, sizing)

	sendErr := make(chan error, 1)
	go func() {
		defer close(done)
		for {
			first, ok := <-completed
			if !ok {
				sendErr <- nil
				return
			}
			responses := []*farmv1.StreamExecuteResponse{first}
			timer := time.NewTimer(streamResponseCoalesceWindow)
		collect:
			for len(responses) < streamBatchMax {
				select {
				case response, open := <-completed:
					if !open {
						break collect
					}
					responses = append(responses, response)
				case <-timer.C:
					break collect
				case <-stream.Context().Done():
					sendErr <- stream.Context().Err()
					return
				}
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if err := stream.Send(&farmv1.StreamExecuteBatchResponse{Responses: responses}); err != nil {
				sendErr <- err
				return
			}
		}
	}()

	var receiveErr error
receive:
	for {
		batch, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			receiveErr = err
			break
		}
		if batch == nil || len(batch.Requests) == 0 || len(batch.Requests) > streamBatchMax {
			receiveErr = status.Error(codes.InvalidArgument, "bad_batch")
			break
		}
		for _, request := range batch.Requests {
			if !scheduler.Submit(request) {
				if err := stream.Context().Err(); err != nil {
					receiveErr = err
				}
				break receive
			}
		}
	}
	scheduler.Wait()
	close(completed)
	if send := <-sendErr; receiveErr == nil {
		receiveErr = send
	}
	return receiveErr
}

func (server *CommandServer) executeBatchRequest(
	ctx context.Context,
	request *farmv1.StreamExecuteRequest,
) *farmv1.StreamExecuteResponse {
	response := &farmv1.StreamExecuteResponse{RequestId: request.GetRequestId()}
	if request == nil || request.RequestId == 0 {
		response.Response = errorClientResponse(request.GetRequest(), errcode.BadRequest)
		return response
	}
	if request.FastSyncUid == 0 {
		if request.Request == nil {
			response.Response = errorClientResponse(nil, errcode.BadRequest)
		} else {
			response.Response = server.execute(ctx, request.Request)
		}
		return response
	}
	if request.Request != nil || request.FastSyncClientSeq == 0 || server.fastSync == nil ||
		server.owns != nil && !server.owns(request.FastSyncUid) {
		response.Response = &farmv1.ClientCommandResponse{
			Envelope: errorEnvelope(204, request.FastSyncClientSeq, errcode.BadRequest),
		}
		return response
	}
	result := server.fastSync.ExecutePreparedSelfSync(ctx, request.FastSyncUid, presence.ConnRef{
		ConnID: request.FastSyncConnId, GatewayID: request.FastSyncGatewayId,
	}, request.FastSyncFromSeq)
	if result.Err != errcode.OK || result.PreparedField != clientwire.PreparedSyncFarmResponse ||
		!result.SyncCaughtUp && len(result.PreparedPayload) == 0 {
		code := result.Err
		if code == errcode.OK {
			code = errcode.Internal
		}
		response.Response = &farmv1.ClientCommandResponse{
			Envelope: errorEnvelope(204, request.FastSyncClientSeq, code),
		}
		return response
	}
	response.FastSyncClientSeq = request.FastSyncClientSeq
	response.FastSyncUid = request.FastSyncUid
	response.FastSyncFarmSeq = result.FarmSeq
	response.FastSyncPreparedPayload = result.PreparedPayload
	response.FastSyncCaughtUp = result.SyncCaughtUp
	response.FastSyncServerTime = result.SyncServerTime
	response.FastSyncTimeProfile = result.SyncTimeProfile
	return response
}

func RegisterCommandService(
	server *grpc.Server,
	executor ClientExecutor,
	owns func(uint64) bool,
	metrics ...*telemetry.Metrics,
) {
	farmv1.RegisterFarmCommandServiceServer(server, NewCommandServer(executor, owns, metrics...))
}
