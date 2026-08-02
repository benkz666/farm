package gateway

import (
	"context"
	"errors"

	"farm/server/platform/actor"
	"farm/server/platform/farm"
	"farm/server/platform/farmrpc"
	"farm/server/platform/gameconf"
	"farm/server/platform/pkgerr"
	"farm/server/platform/pkgjson"
	"farm/server/platform/store"
)

type taskClaimRequest struct {
	TaskID uint32 `json:"task_id"`
}

type mailClaimRequest struct {
	MailID pkgjson.Uint64 `json:"mail_id"`
}

type mailMutationRequest struct {
	MailID pkgjson.Uint64 `json:"mail_id"`
	All    bool           `json:"all"`
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

func (g *Gateway) handleTaskMailRequest(connection *wsConnection, request Envelope) Envelope {
	if request.Cmd == CommandCodexList {
		return g.handleCodexList(connection, request)
	}
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	if g.taskMail == nil {
		response.Err = pkgerr.Internal
		return response
	}

	ctx := context.Background()
	now := g.Now()
	dayKey := gameconf.LocalDayKey(now)
	switch request.Cmd {
	case CommandTaskList:
		if err := unmarshalPayload(request.Payload, &struct{}{}); err != nil {
			response.Err = pkgerr.BadRequest
			return response
		}
		tasks, err := g.taskMail.ListTasks(ctx, connection.uid, dayKey)
		if err != nil {
			response.Err = taskMailErrorCode(err)
			return response
		}
		response.Payload = marshalPayload(taskListResponse{
			Tasks:   tasks,
			ResetAt: gameconf.NextLocalDayResetMs(now),
		})
	case CommandTaskClaim:
		var payload taskClaimRequest
		if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.TaskID == 0 {
			response.Err = pkgerr.BadRequest
			return response
		}
		if g.farmRPC != nil {
			remote, err := g.executeFarmRPC(ctx, connection.uid, farmrpc.CommandRequest{
				Operation:  farmrpc.OperationTaskClaim,
				Originator: g.connectionRef(connection),
				Payload:    marshalPayload(farmrpc.TaskClaimRequest{TaskID: payload.TaskID}),
			})
			if err != nil {
				response.Err = pkgerr.Internal
				return response
			}
			response.Err = remote.Err
			if remote.Err == pkgerr.OK {
				response.Payload = remote.Payload
			}
			return response
		}
		if g.runtime == nil {
			response.Err = pkgerr.Internal
			return response
		}
		var reward store.TaskReward
		var playerDelta farm.PlayerDelta
		var claimErr error
		if err := g.runtime.Do(connection.uid, func(farmActor *actor.FarmActor) error {
			if farmActor == nil || farmActor.Aggregate == nil {
				return errors.New("gateway: actor aggregate is nil")
			}
			reward, claimErr = g.taskMail.ClaimTask(ctx, connection.uid, dayKey, payload.TaskID)
			if claimErr != nil {
				return nil
			}
			farmActor.Aggregate.CreditReward(reward.Coin, reward.Exp)
			playerDelta = farmActor.Aggregate.PlayerDelta()
			return nil
		}); err != nil {
			response.Err = pkgerr.Internal
			return response
		}
		if claimErr != nil {
			response.Err = taskMailErrorCode(claimErr)
			return response
		}
		response.Payload = marshalPayload(reward)
		_ = g.PublishPlayerDelta(ctx, connection.uid, playerDelta)
	case CommandMailList:
		if err := unmarshalPayload(request.Payload, &struct{}{}); err != nil {
			response.Err = pkgerr.BadRequest
			return response
		}
		mails, err := g.taskMail.ListMails(ctx, connection.uid)
		if err != nil {
			response.Err = taskMailErrorCode(err)
			return response
		}
		response.Payload = marshalPayload(mailListResponse{Mails: mails})
	case CommandMailRead, CommandMailDelete:
		var payload mailMutationRequest
		if err := unmarshalPayload(request.Payload, &payload); err != nil ||
			(!payload.All && payload.MailID == 0) ||
			(payload.All && payload.MailID != 0) {
			response.Err = pkgerr.BadRequest
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
		var payload mailClaimRequest
		if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.MailID == 0 {
			response.Err = pkgerr.BadRequest
			return response
		}
		if g.farmRPC != nil {
			remote, err := g.executeFarmRPC(ctx, connection.uid, farmrpc.CommandRequest{
				Operation: farmrpc.OperationMailClaim,
				Payload:   marshalPayload(farmrpc.MailClaimRequest{MailID: uint64(payload.MailID)}),
			})
			if err != nil {
				response.Err = pkgerr.Internal
				return response
			}
			response.Err = remote.Err
			if remote.Err == pkgerr.OK {
				response.Payload = remote.Payload
			}
			return response
		}
		if g.runtime == nil {
			response.Err = pkgerr.Internal
			return response
		}
		var mail store.Mail
		var playerDelta farm.PlayerDelta
		var claimErr error
		if err := g.runtime.Do(connection.uid, func(farmActor *actor.FarmActor) error {
			if farmActor == nil || farmActor.Aggregate == nil {
				return errors.New("gateway: actor aggregate is nil")
			}
			mail, claimErr = g.taskMail.ClaimMail(ctx, connection.uid, uint64(payload.MailID))
			if claimErr != nil {
				return nil
			}
			farmActor.Aggregate.CreditMailReward(mail.AttachmentCoin)
			playerDelta = farmActor.Aggregate.PlayerDelta()
			return nil
		}); err != nil {
			response.Err = pkgerr.Internal
			return response
		}
		if claimErr != nil {
			response.Err = taskMailErrorCode(claimErr)
			return response
		}
		response.Payload = marshalPayload(mail)
		_ = g.PublishPlayerDelta(ctx, connection.uid, playerDelta)
	case CommandClaimDailyLogin:
		if err := unmarshalPayload(request.Payload, &struct{}{}); err != nil {
			response.Err = pkgerr.BadRequest
			return response
		}
		if g.farmRPC != nil {
			remote, err := g.executeFarmRPC(ctx, connection.uid, farmrpc.CommandRequest{
				Operation:  farmrpc.OperationDailyLogin,
				Originator: g.connectionRef(connection),
				Payload:    emptyPayload,
			})
			if err != nil {
				response.Err = pkgerr.Internal
				return response
			}
			response.Err = remote.Err
			if remote.Err == pkgerr.OK {
				response.Payload = remote.Payload
			}
			return response
		}
		if g.runtime == nil {
			response.Err = pkgerr.Internal
			return response
		}
		var reward store.TaskReward
		var playerDelta farm.PlayerDelta
		var claimErr error
		if err := g.runtime.Do(connection.uid, func(farmActor *actor.FarmActor) error {
			if farmActor == nil || farmActor.Aggregate == nil {
				return errors.New("gateway: actor aggregate is nil")
			}
			reward, claimErr = g.taskMail.ClaimDailyLogin(ctx, connection.uid, dayKey)
			if claimErr != nil {
				return nil
			}
			farmActor.Aggregate.CreditReward(reward.Coin, reward.Exp)
			playerDelta = farmActor.Aggregate.PlayerDelta()
			return nil
		}); err != nil {
			response.Err = pkgerr.Internal
			return response
		}
		if claimErr != nil {
			response.Err = dailyLoginErrorCode(claimErr)
			return response
		}
		response.Payload = marshalPayload(reward)
		_ = g.PublishPlayerDelta(ctx, connection.uid, playerDelta)
	default:
		response.Err = pkgerr.BadRequest
	}
	return response
}

func (g *Gateway) handleCodexList(connection *wsConnection, request Envelope) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	if err := unmarshalPayload(request.Payload, &struct{}{}); err != nil {
		response.Err = pkgerr.BadRequest
		return response
	}
	if g.farmRPC != nil {
		remote, err := g.executeFarmRPC(context.Background(), connection.uid, farmrpc.CommandRequest{
			Operation:  farmrpc.OperationCodexList,
			Originator: g.connectionRef(connection),
			Payload:    emptyPayload,
		})
		if err != nil {
			response.Err = pkgerr.Internal
			return response
		}
		response.Err = remote.Err
		if remote.Err == pkgerr.OK {
			response.Payload = remote.Payload
		}
		return response
	}
	if g.runtime == nil {
		response.Err = pkgerr.Internal
		return response
	}
	var entries []farm.CodexProgress
	if err := g.runtime.Do(connection.uid, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("gateway: actor aggregate is nil")
		}
		entries = farmActor.Aggregate.CodexSnapshot()
		return nil
	}); err != nil {
		response.Err = pkgerr.Internal
		return response
	}
	response.Payload = marshalPayload(farmrpc.CodexListResponse{
		Entries: entries,
		Total:   gameconf.CropCount,
	})
	return response
}

func dailyLoginErrorCode(err error) pkgerr.Code {
	if errors.Is(err, store.ErrTaskAlreadyClaimed) || errors.Is(err, store.ErrDailyLoginAlreadyClaimed) {
		return pkgerr.DuplicateOK
	}
	return taskMailErrorCode(err)
}

func taskMailErrorCode(err error) pkgerr.Code {
	switch {
	case errors.Is(err, store.ErrTaskNotComplete):
		return pkgerr.TaskNotComplete
	case errors.Is(err, store.ErrTaskAlreadyClaimed):
		return pkgerr.TaskAlreadyClaimed
	case errors.Is(err, store.ErrMailNotFound):
		return pkgerr.MailNotFound
	case errors.Is(err, store.ErrMailNoAttachment):
		return pkgerr.MailNoAttachment
	case errors.Is(err, store.ErrMailAlreadyClaimed):
		return pkgerr.MailAlreadyClaimed
	case errors.Is(err, store.ErrDailyLoginAlreadyClaimed):
		return pkgerr.DuplicateOK
	default:
		return pkgerr.Internal
	}
}
