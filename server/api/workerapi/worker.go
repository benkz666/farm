// Package workerapi 提供 Worker（任务、邮件、图鉴奖励）服务的内部协议适配器。
package workerapi

import (
	"context"
	"encoding/json"

	"farm/server/api/rpc"
	"farm/server/api/rpcerr"
	"farm/server/platform/farm"
	"farm/server/platform/pkgjson"
	"farm/server/platform/store"
)

const (
	methodListTasks       = "worker.list_tasks"
	methodAdvanceTask     = "worker.advance_task"
	methodClaimTask       = "worker.claim_task"
	methodListMails       = "worker.list_mails"
	methodMarkMailsRead   = "worker.mark_mails_read"
	methodDeleteMails     = "worker.delete_mails"
	methodClaimMail       = "worker.claim_mail"
	methodClaimDailyLogin = "worker.claim_daily_login"
	methodCodexRewards    = "worker.issue_codex_rewards"
)

type request struct {
	UID      pkgjson.UID         `json:"uid"`
	DayKey   int64               `json:"day_key,omitempty"`
	TaskID   uint32              `json:"task_id,omitempty"`
	Amount   uint32              `json:"amount,omitempty"`
	MailID   pkgjson.UID         `json:"mail_id,omitempty"`
	Progress *farm.CodexProgress `json:"progress,omitempty"`
}

type mailDTO struct {
	ID             pkgjson.UID `json:"id"`
	Title          string      `json:"title"`
	AttachmentCoin int64       `json:"attachment_coin"`
	Claimed        bool        `json:"claimed"`
	Read           bool        `json:"read"`
	CreatedAt      int64       `json:"created_at"`
}

// Service 是 Worker 拥有的两组持久化边界。
type Service interface {
	store.TaskMailStore
	store.CodexRewardStore
}

type Dispatcher struct{ service Service }

func NewDispatcher(service Service) *Dispatcher { return &Dispatcher{service: service} }

func (dispatcher *Dispatcher) Dispatch(ctx context.Context, method string, payload json.RawMessage) (any, string) {
	var input request
	if dispatcher == nil || dispatcher.service == nil || json.Unmarshal(payload, &input) != nil || input.UID == 0 {
		return nil, "bad_request"
	}
	uid := uint64(input.UID)
	switch method {
	case methodListTasks:
		value, err := dispatcher.service.ListTasks(ctx, uid, input.DayKey)
		return result(value, err)
	case methodAdvanceTask:
		value, err := dispatcher.service.AdvanceTask(ctx, uid, input.DayKey, input.TaskID, input.Amount)
		return result(value, err)
	case methodClaimTask:
		value, err := dispatcher.service.ClaimTask(ctx, uid, input.DayKey, input.TaskID)
		return result(value, err)
	case methodListMails:
		value, err := dispatcher.service.ListMails(ctx, uid)
		mails := make([]mailDTO, 0, len(value))
		for _, mail := range value {
			mails = append(mails, encodeMail(mail))
		}
		return result(mails, err)
	case methodMarkMailsRead:
		value, err := dispatcher.service.MarkMailsRead(ctx, uid, uint64(input.MailID))
		return result(struct {
			Affected int64 `json:"affected"`
		}{value}, err)
	case methodDeleteMails:
		value, err := dispatcher.service.DeleteMails(ctx, uid, uint64(input.MailID))
		return result(struct {
			Affected int64 `json:"affected"`
		}{value}, err)
	case methodClaimMail:
		value, err := dispatcher.service.ClaimMail(ctx, uid, uint64(input.MailID))
		return result(encodeMail(value), err)
	case methodClaimDailyLogin:
		value, err := dispatcher.service.ClaimDailyLogin(ctx, uid, input.DayKey)
		return result(value, err)
	case methodCodexRewards:
		if input.Progress == nil {
			return nil, "bad_request"
		}
		value, err := dispatcher.service.IssueCodexRewards(ctx, uid, *input.Progress)
		return result(value, err)
	default:
		return nil, "unknown_method"
	}
}

func result(value any, err error) (any, string) {
	if err != nil {
		return nil, rpcerr.Kind(err)
	}
	return value, ""
}

func encodeMail(mail store.Mail) mailDTO {
	return mailDTO{
		ID: pkgjson.UID(mail.ID), Title: mail.Title, AttachmentCoin: mail.AttachmentCoin,
		Claimed: mail.Claimed, Read: mail.Read, CreatedAt: mail.CreatedAt,
	}
}

func decodeMail(mail mailDTO) store.Mail {
	return store.Mail{
		ID: uint64(mail.ID), Title: mail.Title, AttachmentCoin: mail.AttachmentCoin,
		Claimed: mail.Claimed, Read: mail.Read, CreatedAt: mail.CreatedAt,
	}
}

// Client 同时实现 Gateway/Farm 所需的任务邮件与图鉴奖励边界。
type Client struct{ rpc *rpc.Client }

func NewClient(endpoint, internalToken string) *Client {
	return &Client{rpc: rpc.NewClient(endpoint, internalToken, nil)}
}

func (client *Client) call(ctx context.Context, method string, input request, output any) error {
	return rpcerr.Decode(client.rpc.Call(ctx, method, input, output))
}

func baseRequest(uid uint64) request { return request{UID: pkgjson.UID(uid)} }

func (client *Client) ListTasks(ctx context.Context, uid uint64, dayKey int64) ([]store.Task, error) {
	input := baseRequest(uid)
	input.DayKey = dayKey
	var output []store.Task
	err := client.call(ctx, methodListTasks, input, &output)
	return output, err
}

func (client *Client) AdvanceTask(ctx context.Context, uid uint64, dayKey int64, taskID, amount uint32) (store.TaskAdvanceResult, error) {
	input := baseRequest(uid)
	input.DayKey, input.TaskID, input.Amount = dayKey, taskID, amount
	var output store.TaskAdvanceResult
	err := client.call(ctx, methodAdvanceTask, input, &output)
	return output, err
}

func (client *Client) ClaimTask(ctx context.Context, uid uint64, dayKey int64, taskID uint32) (store.TaskReward, error) {
	input := baseRequest(uid)
	input.DayKey, input.TaskID = dayKey, taskID
	var output store.TaskReward
	err := client.call(ctx, methodClaimTask, input, &output)
	return output, err
}

func (client *Client) ListMails(ctx context.Context, uid uint64) ([]store.Mail, error) {
	var output []mailDTO
	if err := client.call(ctx, methodListMails, baseRequest(uid), &output); err != nil {
		return nil, err
	}
	mails := make([]store.Mail, 0, len(output))
	for _, mail := range output {
		mails = append(mails, decodeMail(mail))
	}
	return mails, nil
}

func (client *Client) MarkMailsRead(ctx context.Context, uid, mailID uint64) (int64, error) {
	return client.mailMutation(ctx, methodMarkMailsRead, uid, mailID)
}

func (client *Client) DeleteMails(ctx context.Context, uid, mailID uint64) (int64, error) {
	return client.mailMutation(ctx, methodDeleteMails, uid, mailID)
}

func (client *Client) mailMutation(ctx context.Context, method string, uid, mailID uint64) (int64, error) {
	input := baseRequest(uid)
	input.MailID = pkgjson.UID(mailID)
	var output struct {
		Affected int64 `json:"affected"`
	}
	err := client.call(ctx, method, input, &output)
	return output.Affected, err
}

func (client *Client) ClaimMail(ctx context.Context, uid, mailID uint64) (store.Mail, error) {
	input := baseRequest(uid)
	input.MailID = pkgjson.UID(mailID)
	var output mailDTO
	if err := client.call(ctx, methodClaimMail, input, &output); err != nil {
		return store.Mail{}, err
	}
	return decodeMail(output), nil
}

func (client *Client) ClaimDailyLogin(ctx context.Context, uid uint64, dayKey int64) (store.TaskReward, error) {
	input := baseRequest(uid)
	input.DayKey = dayKey
	var output store.TaskReward
	err := client.call(ctx, methodClaimDailyLogin, input, &output)
	return output, err
}

func (client *Client) IssueCodexRewards(ctx context.Context, uid uint64, progress farm.CodexProgress) ([]farm.CodexRewardNotice, error) {
	input := baseRequest(uid)
	input.Progress = &progress
	var output []farm.CodexRewardNotice
	err := client.call(ctx, methodCodexRewards, input, &output)
	return output, err
}

var (
	_ store.TaskMailStore    = (*Client)(nil)
	_ store.CodexRewardStore = (*Client)(nil)
)
