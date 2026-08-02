// Package authapi 提供 Auth 服务的内部协议适配器。
package authapi

import (
	"context"
	"encoding/json"

	"farm/server/api/rpc"
	"farm/server/api/rpcerr"
	"farm/server/platform/pkgjson"
)

const (
	methodRegister = "auth.register"
	methodLogin    = "auth.login"
)

// Service 是 Auth 服务必须实现的最小业务边界。
type Service interface {
	Register(ctx context.Context, username, password string) (uint64, string, error)
	Login(ctx context.Context, username, password string) (uint64, string, error)
}

type credentialRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type sessionResponse struct {
	UID   pkgjson.UID `json:"uid"`
	Token string      `json:"token"`
}

// Dispatcher 把内部 RPC 请求转给认证用例。
type Dispatcher struct {
	service Service
}

func NewDispatcher(service Service) *Dispatcher { return &Dispatcher{service: service} }

func (dispatcher *Dispatcher) Dispatch(ctx context.Context, method string, payload json.RawMessage) (any, string) {
	var request credentialRequest
	if dispatcher == nil || dispatcher.service == nil || json.Unmarshal(payload, &request) != nil || request.Username == "" || request.Password == "" {
		return nil, "bad_request"
	}
	var (
		uid   uint64
		token string
		err   error
	)
	switch method {
	case methodRegister:
		uid, token, err = dispatcher.service.Register(ctx, request.Username, request.Password)
	case methodLogin:
		uid, token, err = dispatcher.service.Login(ctx, request.Username, request.Password)
	default:
		return nil, "unknown_method"
	}
	if err != nil {
		return nil, rpcerr.Kind(err)
	}
	return sessionResponse{UID: pkgjson.UID(uid), Token: token}, ""
}

// Client 实现 Gateway 所需的认证边界。
type Client struct {
	rpc *rpc.Client
}

func NewClient(endpoint, internalToken string) *Client {
	return &Client{rpc: rpc.NewClient(endpoint, internalToken, nil)}
}

func (client *Client) Register(ctx context.Context, username, password string) (uint64, string, error) {
	return client.authenticate(ctx, methodRegister, username, password)
}

func (client *Client) Login(ctx context.Context, username, password string) (uint64, string, error) {
	return client.authenticate(ctx, methodLogin, username, password)
}

func (client *Client) authenticate(ctx context.Context, method, username, password string) (uint64, string, error) {
	var response sessionResponse
	err := client.rpc.Call(ctx, method, credentialRequest{Username: username, Password: password}, &response)
	if err != nil {
		return 0, "", rpcerr.Decode(err)
	}
	return uint64(response.UID), response.Token, nil
}
