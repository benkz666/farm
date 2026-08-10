package gateway

import (
	"context"
	"encoding/json"
	"errors"

	"farm/server/domain/farm"
	"farm/server/farmsvr/farmrpc"
	"farm/server/farmsvr/room"
	"farm/server/shared/clientjson"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
	"farm/server/shared/store"
)

type taskClaimRequest struct {
	TaskID uint32 `json:"task_id"`
}

type mailClaimRequest struct {
	MailID clientjson.Uint64 `json:"mail_id"`
}

type mailMutationRequest struct {
	MailID clientjson.Uint64 `json:"mail_id"`
	All    bool              `json:"all"`
}

type taskListResponse struct {
	Tasks   []store.Task `json:"tasks"`
	ResetAt int64        `json:"reset_at"`
}

type mailListResponse struct {
	Mails []store.Mail `json:"mails"`
}

type mailMutationResponse struct {
	Affected int64 `json:"affected"`
}

func validateEmptyClientRequest(request Envelope) error {
	if request.CommandRequest != nil {
		return nil
	}
	return unmarshalPayload(request.Payload, &struct{}{})
}

func decodeTaskClaimClientRequest(request Envelope) (taskClaimRequest, error) {
	if request.CommandRequest != nil {
		return taskClaimRequest{TaskID: request.CommandRequest.TaskId}, nil
	}
	var value taskClaimRequest
	err := unmarshalPayload(request.Payload, &value)
	return value, err
}

func decodeMailMutationClientRequest(request Envelope) (mailMutationRequest, error) {
	if request.CommandRequest != nil {
		return mailMutationRequest{MailID: clientjson.Uint64(request.CommandRequest.MailId), All: request.CommandRequest.All}, nil
	}
	var value mailMutationRequest
	err := unmarshalPayload(request.Payload, &value)
	return value, err
}

func decodeMailClaimClientRequest(request Envelope) (mailClaimRequest, error) {
	if request.CommandRequest != nil {
		return mailClaimRequest{MailID: clientjson.Uint64(request.CommandRequest.MailId)}, nil
	}
	var value mailClaimRequest
	err := unmarshalPayload(request.Payload, &value)
	return value, err
}

func (g *Gateway) handleTaskMailRequest(connection *wsConnection, request Envelope) Envelope {
	if request.Cmd == CommandCodexList {
		return g.handleCodexList(connection, request)
	}
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}

	ctx := context.Background()
	now := g.Now()
	dayKey := gameconfig.LocalDayKey(now)

	if g.farmRPC != nil {
		return g.handleTaskMailRemote(connection, request, ctx)
	}
	if g.taskMail == nil {
		response.Err = errcode.Internal
		return response
	}

	switch request.Cmd {
	case CommandTaskList:
		if err := validateEmptyClientRequest(request); err != nil {
			response.Err = errcode.BadRequest
			return response
		}
		tasks, err := g.taskMail.ListTasks(ctx, connection.uid, dayKey)
		if err != nil {
			response.Err = taskMailErrorCode(err)
			return response
		}
		response.Payload = marshalPayload(taskListResponse{
			Tasks:   tasks,
			ResetAt: gameconfig.NextLocalDayResetMs(now),
		})
	case CommandTaskClaim:
		payload, decodeErr := decodeTaskClaimClientRequest(request)
		if decodeErr != nil || payload.TaskID == 0 {
			response.Err = errcode.BadRequest
			return response
		}
		if g.runtime == nil {
			response.Err = errcode.Internal
			return response
		}
		var reward store.TaskReward
		var playerDelta farm.PlayerDelta
		var claimErr error
		if err := g.runtime.Do(connection.uid, func(farmActor *room.FarmActor) error {
			if farmActor == nil || farmActor.Aggregate == nil {
				return errors.New("gateway: actor aggregate is nil")
			}
			reward, claimErr = g.taskMail.ClaimTask(ctx, connection.uid, dayKey, payload.TaskID)
			if claimErr != nil {
				return nil
			}
			// 低频例外：ClaimTask 在 Actor 锁内先写 MySQL 再同步内存。
			farmActor.Aggregate.CreditReward(reward.Coin, reward.Exp)
			farmActor.MarkDirty()
			playerDelta = farmActor.Aggregate.PlayerDelta()
			return nil
		}); err != nil {
			response.Err = errcode.Internal
			return response
		}
		if claimErr != nil {
			response.Err = taskMailErrorCode(claimErr)
			return response
		}
		response.Payload = marshalPayload(reward)
		_ = g.PublishPlayerDelta(ctx, connection.uid, playerDelta)
	case CommandMailList:
		if err := validateEmptyClientRequest(request); err != nil {
			response.Err = errcode.BadRequest
			return response
		}
		mails, err := g.taskMail.ListMails(ctx, connection.uid)
		if err != nil {
			response.Err = taskMailErrorCode(err)
			return response
		}
		response.Payload = marshalPayload(mailListResponse{Mails: mails})
	case CommandMailRead, CommandMailDelete:
		payload, decodeErr := decodeMailMutationClientRequest(request)
		if decodeErr != nil ||
			(!payload.All && payload.MailID == 0) ||
			(payload.All && payload.MailID != 0) {
			response.Err = errcode.BadRequest
			return response
		}
		mailID := uint64(payload.MailID)
		var (
			affected int64
			err      error
		)
		if request.Cmd == CommandMailRead {
			affected, err = g.taskMail.MarkMailsRead(ctx, connection.uid, mailID)
		} else {
			affected, err = g.taskMail.DeleteMails(ctx, connection.uid, mailID)
		}
		if err != nil {
			response.Err = taskMailErrorCode(err)
			return response
		}
		response.Payload = marshalPayload(mailMutationResponse{Affected: affected})
	case CommandMailClaim:
		payload, decodeErr := decodeMailClaimClientRequest(request)
		if decodeErr != nil || payload.MailID == 0 {
			response.Err = errcode.BadRequest
			return response
		}
		if g.runtime == nil {
			response.Err = errcode.Internal
			return response
		}
		var mail store.Mail
		var playerDelta farm.PlayerDelta
		var claimErr error
		if err := g.runtime.Do(connection.uid, func(farmActor *room.FarmActor) error {
			if farmActor == nil || farmActor.Aggregate == nil {
				return errors.New("gateway: actor aggregate is nil")
			}
			mail, claimErr = g.taskMail.ClaimMail(ctx, connection.uid, uint64(payload.MailID))
			if claimErr != nil {
				return nil
			}
			// 低频例外：ClaimMail 在 Actor 锁内先写 MySQL 再同步内存。
			farmActor.Aggregate.CreditMailReward(mail.AttachmentCoin)
			farmActor.MarkDirty()
			playerDelta = farmActor.Aggregate.PlayerDelta()
			return nil
		}); err != nil {
			response.Err = errcode.Internal
			return response
		}
		if claimErr != nil {
			response.Err = taskMailErrorCode(claimErr)
			return response
		}
		response.Payload = marshalPayload(mail)
		_ = g.PublishPlayerDelta(ctx, connection.uid, playerDelta)
	case CommandClaimDailyLogin:
		if err := validateEmptyClientRequest(request); err != nil {
			response.Err = errcode.BadRequest
			return response
		}
		if g.runtime == nil {
			response.Err = errcode.Internal
			return response
		}
		var reward store.TaskReward
		var playerDelta farm.PlayerDelta
		var claimErr error
		if err := g.runtime.Do(connection.uid, func(farmActor *room.FarmActor) error {
			if farmActor == nil || farmActor.Aggregate == nil {
				return errors.New("gateway: actor aggregate is nil")
			}
			reward, claimErr = g.taskMail.ClaimDailyLogin(ctx, connection.uid, dayKey)
			if claimErr != nil {
				return nil
			}
			farmActor.Aggregate.CreditReward(reward.Coin, reward.Exp)
			farmActor.MarkDirty()
			playerDelta = farmActor.Aggregate.PlayerDelta()
			return nil
		}); err != nil {
			response.Err = errcode.Internal
			return response
		}
		if claimErr != nil {
			response.Err = dailyLoginErrorCode(claimErr)
			return response
		}
		response.Payload = marshalPayload(reward)
		_ = g.PublishPlayerDelta(ctx, connection.uid, playerDelta)
	default:
		response.Err = errcode.BadRequest
	}
	return response
}

func (g *Gateway) handleTaskMailRemote(connection *wsConnection, request Envelope, ctx context.Context) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}

	var (
		operation farmrpc.Operation
		payload   json.RawMessage
	)
	switch request.Cmd {
	case CommandTaskList:
		if err := validateEmptyClientRequest(request); err != nil {
			response.Err = errcode.BadRequest
			return response
		}
		operation = farmrpc.OperationTaskList
		payload = emptyPayload
	case CommandTaskClaim:
		body, decodeErr := decodeTaskClaimClientRequest(request)
		if decodeErr != nil || body.TaskID == 0 {
			response.Err = errcode.BadRequest
			return response
		}
		operation = farmrpc.OperationTaskClaim
		payload = marshalPayload(farmrpc.TaskClaimRequest{TaskID: body.TaskID})
	case CommandMailList:
		if err := validateEmptyClientRequest(request); err != nil {
			response.Err = errcode.BadRequest
			return response
		}
		operation = farmrpc.OperationMailList
		payload = emptyPayload
	case CommandMailRead:
		body, decodeErr := decodeMailMutationClientRequest(request)
		if decodeErr != nil ||
			(!body.All && body.MailID == 0) ||
			(body.All && body.MailID != 0) {
			response.Err = errcode.BadRequest
			return response
		}
		operation = farmrpc.OperationMailRead
		payload = marshalPayload(farmrpc.MailMutationRequest{
			MailID: uint64(body.MailID),
			All:    body.All,
		})
	case CommandMailDelete:
		body, decodeErr := decodeMailMutationClientRequest(request)
		if decodeErr != nil ||
			(!body.All && body.MailID == 0) ||
			(body.All && body.MailID != 0) {
			response.Err = errcode.BadRequest
			return response
		}
		operation = farmrpc.OperationMailDelete
		payload = marshalPayload(farmrpc.MailMutationRequest{
			MailID: uint64(body.MailID),
			All:    body.All,
		})
	case CommandMailClaim:
		body, decodeErr := decodeMailClaimClientRequest(request)
		if decodeErr != nil || body.MailID == 0 {
			response.Err = errcode.BadRequest
			return response
		}
		operation = farmrpc.OperationMailClaim
		payload = marshalPayload(farmrpc.MailClaimRequest{MailID: uint64(body.MailID)})
	case CommandClaimDailyLogin:
		if err := validateEmptyClientRequest(request); err != nil {
			response.Err = errcode.BadRequest
			return response
		}
		operation = farmrpc.OperationDailyLogin
		payload = emptyPayload
	default:
		response.Err = errcode.BadRequest
		return response
	}

	remote, err := g.executeFarmRPC(ctx, connection.uid, farmrpc.CommandRequest{
		Operation:     operation,
		Originator:    g.connectionRef(connection),
		ClientCommand: request.Cmd,
		ClientRequest: request.CommandRequest,
		Payload:       payload,
	})
	if err != nil {
		response.Err = errcode.Internal
		return response
	}
	response.Err = remote.Err
	if remote.Err == errcode.OK {
		response.Payload = remote.Payload
		response.CommandResponse = remote.ClientResponse
	}
	return response
}

func (g *Gateway) handleCodexList(connection *wsConnection, request Envelope) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	if err := validateEmptyClientRequest(request); err != nil {
		response.Err = errcode.BadRequest
		return response
	}
	if g.farmRPC != nil {
		remote, err := g.executeFarmRPC(context.Background(), connection.uid, farmrpc.CommandRequest{
			Operation:     farmrpc.OperationCodexList,
			Originator:    g.connectionRef(connection),
			ClientCommand: request.Cmd,
			ClientRequest: request.CommandRequest,
			Payload:       emptyPayload,
		})
		if err != nil {
			response.Err = errcode.Internal
			return response
		}
		response.Err = remote.Err
		if remote.Err == errcode.OK {
			response.Payload = remote.Payload
			response.CommandResponse = remote.ClientResponse
		}
		return response
	}
	if g.runtime == nil {
		response.Err = errcode.Internal
		return response
	}
	var entries []farm.CodexProgress
	if err := g.runtime.Do(connection.uid, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("gateway: actor aggregate is nil")
		}
		entries = farmActor.Aggregate.CodexSnapshot()
		return nil
	}); err != nil {
		response.Err = errcode.Internal
		return response
	}
	response.Payload = marshalPayload(farmrpc.CodexListResponse{
		Entries: entries,
		Total:   gameconfig.CropCount,
	})
	return response
}

func dailyLoginErrorCode(err error) errcode.Code {
	if errors.Is(err, store.ErrTaskAlreadyClaimed) || errors.Is(err, store.ErrDailyLoginAlreadyClaimed) {
		return errcode.DuplicateOK
	}
	return taskMailErrorCode(err)
}

func taskMailErrorCode(err error) errcode.Code {
	switch {
	case errors.Is(err, store.ErrTaskNotComplete):
		return errcode.TaskNotComplete
	case errors.Is(err, store.ErrTaskAlreadyClaimed):
		return errcode.TaskAlreadyClaimed
	case errors.Is(err, store.ErrMailNotFound):
		return errcode.MailNotFound
	case errors.Is(err, store.ErrMailNoAttachment):
		return errcode.MailNoAttachment
	case errors.Is(err, store.ErrMailAlreadyClaimed):
		return errcode.MailAlreadyClaimed
	case errors.Is(err, store.ErrDailyLoginAlreadyClaimed):
		return errcode.DuplicateOK
	default:
		return errcode.Internal
	}
}
