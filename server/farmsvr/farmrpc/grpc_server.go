package farmrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"farm/server/gateway/presence"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	streamWorkerCount = 64
	streamWorkerQueue = 64
	streamBatchMax    = 64
	// Give completed commands a very small window to form an efficient response
	// batch without making fast commands wait for the slowest command in their
	// original request batch.
	streamResponseCoalesceWindow = 50 * time.Microsecond
)

// ExecuteBatchStream executes batches over one long-lived stream. Commands
// are still assigned by farm_uid to stable workers, so batches do not weaken
// same-farm ordering.
func (server *CommandServer) ExecuteBatchStream(stream farmv1.FarmCommandService_ExecuteBatchStreamServer) error {
	type workItem struct {
		request *farmv1.StreamExecuteRequest
	}

	workers := make([]chan workItem, streamWorkerCount)
	completed := make(chan *farmv1.StreamExecuteResponse, streamWorkerCount*2)
	sendStopped := make(chan struct{})
	var workerWG sync.WaitGroup
	for index := range workers {
		workers[index] = make(chan workItem, streamWorkerQueue)
		workerWG.Add(1)
		go func(queue <-chan workItem) {
			defer workerWG.Done()
			for item := range queue {
				request := item.request
				response := &farmv1.StreamExecuteResponse{RequestId: request.GetRequestId()}
				if request == nil || request.RequestId == 0 || request.Request == nil {
					response.Response = &farmv1.ExecuteResponse{Err: int32(errcode.BadRequest)}
				} else {
					response.Response = server.execute(request.Request)
				}
				select {
				case completed <- response:
				case <-sendStopped:
					return
				case <-stream.Context().Done():
					return
				}
			}
		}(workers[index])
	}

	sendErr := make(chan error, 1)
	go func() {
		defer close(sendStopped)
		for {
			first, ok := <-completed
			if !ok {
				sendErr <- nil
				return
			}
			responses := make([]*farmv1.StreamExecuteResponse, 0, streamBatchMax)
			responses = append(responses, first)
			timer := time.NewTimer(streamResponseCoalesceWindow)
			closed := false
		collect:
			for len(responses) < streamBatchMax {
				select {
				case response, open := <-completed:
					if !open {
						closed = true
						break collect
					}
					responses = append(responses, response)
				case <-timer.C:
					break collect
				case <-stream.Context().Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
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
			if closed {
				sendErr <- nil
				return
			}
		}
	}()

	var (
		receiveErr    error
		streamSendErr error
		sendFinished  bool
	)
receiveLoop:
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
			workerIndex := 0
			if request != nil && request.Request != nil {
				workerIndex = int(request.Request.FarmUid % uint64(len(workers)))
			}
			select {
			case workers[workerIndex] <- workItem{request: request}:
			case <-sendStopped:
				streamSendErr = <-sendErr
				sendFinished = true
				receiveErr = streamSendErr
				break receiveLoop
			case <-stream.Context().Done():
				receiveErr = stream.Context().Err()
				break receiveLoop
			}
		}
	}
	for _, worker := range workers {
		close(worker)
	}
	workerWG.Wait()
	close(completed)
	if !sendFinished {
		streamSendErr = <-sendErr
	}
	if streamSendErr != nil && receiveErr == nil {
		return streamSendErr
	}
	return receiveErr
}

// CommandServer implements FarmCommandService over a transport-neutral Handler.
type CommandServer struct {
	farmv1.UnimplementedFarmCommandServiceServer
	handler *Handler
	owns    func(uint64) bool
}

// NewCommandServer registers the farm command executor behind gRPC.
func NewCommandServer(handler *Handler, owns func(uint64) bool) *CommandServer {
	return &CommandServer{handler: handler, owns: owns}
}

// Execute routes one Gateway-authorized command to the local farm runtime.
func (server *CommandServer) Execute(_ context.Context, request *farmv1.ExecuteRequest) (*farmv1.ExecuteResponse, error) {
	return server.execute(request), nil
}

// ExecuteStream multiplexes commands over one long-lived HTTP/2 stream. A
// stable uid shard preserves the same-farm ordering guarantee while allowing
// unrelated farms to execute concurrently.
func (server *CommandServer) ExecuteStream(stream farmv1.FarmCommandService_ExecuteStreamServer) error {
	type workItem struct {
		requestID uint64
		request   *farmv1.ExecuteRequest
	}

	workers := make([]chan workItem, streamWorkerCount)
	responses := make(chan *farmv1.StreamExecuteResponse, streamWorkerCount*2)
	var workerWG sync.WaitGroup
	for index := range workers {
		workers[index] = make(chan workItem, streamWorkerQueue)
		workerWG.Add(1)
		go func(queue <-chan workItem) {
			defer workerWG.Done()
			for item := range queue {
				response := &farmv1.StreamExecuteResponse{
					RequestId: item.requestID,
					Response:  server.execute(item.request),
				}
				select {
				case responses <- response:
				case <-stream.Context().Done():
					return
				}
			}
		}(workers[index])
	}

	sendErr := make(chan error, 1)
	go func() {
		for response := range responses {
			if err := stream.Send(response); err != nil {
				sendErr <- err
				return
			}
		}
		sendErr <- nil
	}()

	var receiveErr error
	for {
		request, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			receiveErr = err
			break
		}
		if request == nil || request.RequestId == 0 || request.Request == nil {
			responses <- &farmv1.StreamExecuteResponse{
				RequestId: request.GetRequestId(),
				Response:  &farmv1.ExecuteResponse{Err: int32(errcode.BadRequest)},
			}
			continue
		}
		queue := workers[request.Request.FarmUid%uint64(len(workers))]
		select {
		case queue <- workItem{requestID: request.RequestId, request: request.Request}:
		case err := <-sendErr:
			for _, worker := range workers {
				close(worker)
			}
			workerWG.Wait()
			close(responses)
			return err
		case <-stream.Context().Done():
			receiveErr = stream.Context().Err()
			break
		}
		if receiveErr != nil {
			break
		}
	}
	for _, worker := range workers {
		close(worker)
	}
	workerWG.Wait()
	close(responses)
	if err := <-sendErr; err != nil {
		return err
	}
	return receiveErr
}

func (server *CommandServer) execute(request *farmv1.ExecuteRequest) *farmv1.ExecuteResponse {
	if server == nil || server.handler == nil {
		return &farmv1.ExecuteResponse{Err: int32(errcode.Internal)}
	}
	if request == nil || request.FarmUid == 0 || server.owns != nil && !server.owns(request.FarmUid) {
		return &farmv1.ExecuteResponse{Err: int32(errcode.BadRequest)}
	}
	operation, ok := operationFromProtoEnum(request.Operation)
	if !ok {
		return &farmv1.ExecuteResponse{Err: int32(errcode.BadRequest)}
	}
	response := server.handler.Execute(CommandRequest{
		Operation:      operation,
		FarmUID:        request.FarmUid,
		Originator:     connRefFromProto(request.Originator),
		Payload:        json.RawMessage(request.PayloadJson),
		PreferPrepared: request.PreferPrepared,
	})
	payloadJSON := response.Payload
	preparedPayload := response.PreparedPayload
	preparedField := response.PreparedField
	if request.PreferPrepared && len(preparedPayload) > 0 {
		payloadJSON = nil
	} else {
		preparedPayload = nil
		preparedField = 0
	}
	return &farmv1.ExecuteResponse{
		Err:             int32(response.Err),
		PayloadJson:     payloadJSON,
		FarmSeq:         response.FarmSeq,
		PreparedPayload: preparedPayload,
		PreparedField:   preparedField,
	}
}

func connRefFromProto(ref *farmv1.ConnRef) presence.ConnRef {
	if ref == nil {
		return presence.ConnRef{}
	}
	return presence.ConnRef{ConnID: ref.ConnId, GatewayID: ref.GatewayId}
}

func connRefToProto(ref presence.ConnRef) *farmv1.ConnRef {
	if ref.ConnID == 0 && ref.GatewayID == "" {
		return nil
	}
	return &farmv1.ConnRef{ConnId: ref.ConnID, GatewayId: ref.GatewayID}
}
