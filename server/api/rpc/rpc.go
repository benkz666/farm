// Package rpc 定义五个服务之间共用的轻量 JSON-RPC 信封。
//
// 它只负责传输，不包含任何业务类型。这样协议模块不会反向依赖某个服务的
// 数据库模型，业务请求与响应仍由各服务自行定义和校验。
package rpc

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// Path 是所有服务内部调用的统一入口。
	Path = "/internal/v1/rpc"
	// maxBodySize 防止内部接口因异常请求无限占用内存。
	maxBodySize = 1 << 20
)

// Request 是服务间调用信封。Payload 的具体结构由 Method 决定。
type Request struct {
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload"`
}

// Response 是服务间响应信封。ErrorKind 非空表示调用失败。
type Response struct {
	Payload   json.RawMessage `json:"payload,omitempty"`
	ErrorKind string          `json:"error_kind,omitempty"`
}

// RemoteError 表示对端已经正常处理请求，但业务执行失败。
type RemoteError struct {
	Kind string
}

func (err *RemoteError) Error() string { return "remote service: " + err.Kind }

// Dispatcher 由各服务实现，把 method 分派到明确的业务用例。
type Dispatcher interface {
	Dispatch(ctx context.Context, method string, payload json.RawMessage) (any, string)
}

// NewHandler 创建带内部令牌校验、严格 JSON 校验与请求体上限的 RPC 处理器。
func NewHandler(internalToken string, dispatcher Dispatcher) http.Handler {
	wantAuthorization := []byte("Bearer " + strings.TrimSpace(internalToken))
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writeResponse(w, http.StatusMethodNotAllowed, Response{ErrorKind: "method_not_allowed"})
			return
		}
		if len(wantAuthorization) == len("Bearer ") || subtle.ConstantTimeCompare(
			[]byte(request.Header.Get("Authorization")), wantAuthorization,
		) != 1 {
			writeResponse(w, http.StatusUnauthorized, Response{ErrorKind: "unauthorized"})
			return
		}

		var envelope Request
		decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, maxBodySize))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&envelope); err != nil || strings.TrimSpace(envelope.Method) == "" {
			writeResponse(w, http.StatusBadRequest, Response{ErrorKind: "bad_request"})
			return
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			writeResponse(w, http.StatusBadRequest, Response{ErrorKind: "bad_request"})
			return
		}

		result, errorKind := dispatcher.Dispatch(request.Context(), envelope.Method, envelope.Payload)
		if errorKind != "" {
			writeResponse(w, http.StatusOK, Response{ErrorKind: errorKind})
			return
		}
		payload, err := json.Marshal(result)
		if err != nil {
			writeResponse(w, http.StatusInternalServerError, Response{ErrorKind: "internal"})
			return
		}
		writeResponse(w, http.StatusOK, Response{Payload: payload})
	})
}

func writeResponse(w http.ResponseWriter, status int, response Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

// Client 是线程安全的内部 RPC 客户端。
type Client struct {
	endpoint      string
	internalToken string
	httpClient    *http.Client
}

// NewClient 创建客户端。endpoint 必须是服务根地址，不包含 RPC 路径。
func NewClient(endpoint, internalToken string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{
		endpoint:      strings.TrimRight(strings.TrimSpace(endpoint), "/") + Path,
		internalToken: strings.TrimSpace(internalToken),
		httpClient:    httpClient,
	}
}

// Call 发起一次调用并严格解码响应。业务错误以 *RemoteError 返回。
func (client *Client) Call(ctx context.Context, method string, input, output any) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("rpc: encode %s request: %w", method, err)
	}
	body, err := json.Marshal(Request{Method: method, Payload: payload})
	if err != nil {
		return fmt.Errorf("rpc: encode %s envelope: %w", method, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("rpc: create %s request: %w", method, err)
	}
	request.Header.Set("Authorization", "Bearer "+client.internalToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("rpc: call %s: %w", method, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("rpc: call %s returned HTTP %d", method, response.StatusCode)
	}
	var envelope Response
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxBodySize))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("rpc: decode %s response: %w", method, err)
	}
	if envelope.ErrorKind != "" {
		return &RemoteError{Kind: envelope.ErrorKind}
	}
	if output == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Payload, output); err != nil {
		return fmt.Errorf("rpc: decode %s payload: %w", method, err)
	}
	return nil
}
