package farmrpc

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	streamWorkerCount            = 64
	streamWorkerQueue            = 64
	streamBatchMax               = 64
	streamResponseCoalesceWindow = 50 * time.Microsecond
)

// ClientExecutor owns command validation and all Farm business behavior.
type ClientExecutor interface {
	ExecuteClient(context.Context, *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse
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
	tasks    TaskAdvancer
	owns     func(uint64) bool
}

func NewCommandServer(executor ClientExecutor, owns func(uint64) bool) *CommandServer {
	tasks, _ := executor.(TaskAdvancer)
	return &CommandServer{executor: executor, tasks: tasks, owns: owns}
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
	type workItem struct{ request *farmv1.StreamExecuteRequest }
	workers := make([]chan workItem, streamWorkerCount)
	completed := make(chan *farmv1.StreamExecuteResponse, streamWorkerCount*2)
	done := make(chan struct{})
	var workersWG sync.WaitGroup
	for index := range workers {
		workers[index] = make(chan workItem, streamWorkerQueue)
		workersWG.Add(1)
		go func(queue <-chan workItem) {
			defer workersWG.Done()
			for item := range queue {
				request := item.request
				response := &farmv1.StreamExecuteResponse{RequestId: request.GetRequestId()}
				if request == nil || request.RequestId == 0 || request.Request == nil {
					response.Response = errorClientResponse(request.GetRequest(), errcode.BadRequest)
				} else {
					response.Response = server.execute(stream.Context(), request.Request)
				}
				select {
				case completed <- response:
				case <-done:
					return
				case <-stream.Context().Done():
					return
				}
			}
		}(workers[index])
	}

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
			index := 0
			if request != nil && request.Request != nil {
				index = int(request.Request.RouteUid % uint64(len(workers)))
			}
			select {
			case workers[index] <- workItem{request: request}:
			case <-done:
				break receive
			case <-stream.Context().Done():
				receiveErr = stream.Context().Err()
				break receive
			}
		}
	}
	for _, worker := range workers {
		close(worker)
	}
	workersWG.Wait()
	close(completed)
	if send := <-sendErr; receiveErr == nil {
		receiveErr = send
	}
	return receiveErr
}

func RegisterCommandService(server *grpc.Server, executor ClientExecutor, owns func(uint64) bool) {
	farmv1.RegisterFarmCommandServiceServer(server, NewCommandServer(executor, owns))
}
