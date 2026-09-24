package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/basispoints"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Basispoints（Excel 插件画像）通道：命中 extra.openai_basispoints 的 OpenAI
// OAuth 账号把 /v1/responses 改写为 Basispoints 严格白名单 schema 后送往
// bps.openai.com，实测返回真 gpt-6-astra（绕过 chatgpt.com/backend-api/codex
// 的降智调度）。tools 经 run_officejs transport envelope 偷渡，响应整流后
// 再合成标准 Responses SSE/JSON 回给客户端。
const (
	extraKeyOpenAIBasisPoints             = "openai_basispoints"
	extraKeyOpenAIBasisPointsModel        = "openai_basispoints_model"
	extraKeyOpenAIBasisPointsURL          = "openai_basispoints_url"
	extraKeyOpenAIBasisPointsTimezone     = "openai_basispoints_timezone"
	extraKeyOpenAIBasisPointsToolsVerID   = "openai_basispoints_tools_version_id"
	extraKeyOpenAIBasisPointsTimeoutSecs  = "openai_basispoints_timeout_seconds"
	openAIBasisPointsUpstreamEndpoint     = "/basispoints/api/responses"
	openAIBasisPointsResponseReadLimitPad = 1 << 10
)

// IsOpenAIBasisPointsEnabled 报告 OpenAI OAuth/SetupToken 账号是否启用
// Basispoints 上游通道。字段：accounts.extra.openai_basispoints（bool）。
func (a *Account) IsOpenAIBasisPointsEnabled() bool {
	if a == nil || !a.IsOpenAIOAuthLike() || a.Extra == nil {
		return false
	}
	enabled, ok := a.Extra[extraKeyOpenAIBasisPoints].(bool)
	return ok && enabled
}

// resolveOpenAIBasisPointsConfig 从账号 Extra 解析 Basispoints 通道配置。
// upstream model 优先级：openai_basispoints_model > 账号 model_mapping 结果 >
// gpt-6-astra 默认值。
func (a *Account) resolveOpenAIBasisPointsConfig(requestedModel string) basispoints.Config {
	cfg := basispoints.DefaultConfig()
	if a == nil {
		return cfg
	}
	if v := strings.TrimSpace(a.GetExtraString(extraKeyOpenAIBasisPointsURL)); v != "" {
		cfg.ResponsesURL = v
	}
	if v := strings.TrimSpace(a.GetExtraString(extraKeyOpenAIBasisPointsModel)); v != "" {
		cfg.UpstreamModel = v
	} else if mapped := strings.TrimSpace(a.GetMappedModel(requestedModel)); mapped != "" {
		cfg.UpstreamModel = mapped
	}
	cfg.Timezone = strings.TrimSpace(a.GetExtraString(extraKeyOpenAIBasisPointsTimezone))
	cfg.ToolsVersionID = strings.TrimSpace(a.GetExtraString(extraKeyOpenAIBasisPointsToolsVerID))
	if secs := openAIBasisPointsExtraInt64(a, extraKeyOpenAIBasisPointsTimeoutSecs); secs > 0 {
		cfg.TimeoutSeconds = int(secs)
	}
	return cfg
}

func openAIBasisPointsExtraInt64(a *Account, key string) int64 {
	if a == nil || a.Extra == nil {
		return 0
	}
	switch v := a.Extra[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	}
	return 0
}

// resolveBasisPointsAccountID 取 ChatGPT account id：优先凭据字段，缺失时
// 从 access_token JWT claim 解析。影子账号先回源到承载凭据的父账号。
func (s *OpenAIGatewayService) resolveBasisPointsAccountID(ctx context.Context, account *Account, accessToken string) string {
	credAccount := account
	if account.IsShadow() && s.accountRepo != nil {
		if resolved, err := resolveCredentialAccount(ctx, s.accountRepo, account); err == nil && resolved != nil {
			credAccount = resolved
		}
	}
	for _, key := range []string{"chatgpt_account_id", "account_id"} {
		if v := strings.TrimSpace(credAccount.GetCredential(key)); v != "" {
			return v
		}
	}
	return basispoints.AccountIDFromToken(accessToken)
}

// forwardOpenAIBasisPoints 是 /v1/responses 在 Basispoints 通道上的转发实现：
// 客户端 Responses body → 严格白名单 schema（剥 tools→developer 目录、
// instructions→developer、reasoning→reasoning_effort、metadata 重建）→
// Excel 画像头 → 整流上游 SSE → run_officejs 回译 → 合成下游响应。
func (s *OpenAIGatewayService) forwardOpenAIBasisPoints(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestedModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	reqStream := gjson.GetBytes(body, "stream").Bool()

	source, err := decodeBasisPointsObject(body)
	if err != nil {
		return nil, err
	}
	cfg := account.resolveOpenAIBasisPointsConfig(requestedModel)
	if err := cfg.Normalize(); err != nil {
		return nil, fmt.Errorf("basispoints config: %w", err)
	}
	upstreamModel := cfg.UpstreamModel
	SetOpsUpstreamModel(c, upstreamModel)

	upstreamBody, err := basispoints.PrepareResponsesBody(source, cfg)
	if err != nil {
		return nil, fmt.Errorf("prepare basispoints request body: %w", err)
	}
	payload, err := marshalOpenAIUpstreamJSON(upstreamBody)
	if err != nil {
		return nil, fmt.Errorf("serialize basispoints request body: %w", err)
	}

	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	accountID := s.resolveBasisPointsAccountID(ctx, account, token)
	if accountID == "" {
		return nil, fmt.Errorf("basispoints: chatgpt account id missing in credentials and access token claims")
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()
	reqCtx := upstreamCtx
	if cfg.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(upstreamCtx, time.Duration(cfg.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	upstreamReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.ResponsesURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	upstreamReq = upstreamReq.WithContext(WithHTTPUpstreamProfile(upstreamReq.Context(), HTTPUpstreamProfileOpenAI))
	for key, values := range basispoints.AuthHeaders(token, accountID, reqStream, cfg) {
		for _, value := range values {
			upstreamReq.Header.Add(key, value)
		}
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.doOpenAIUpstream(upstreamReq, proxyURL, account)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(respBody)))
		if s.shouldFailoverOpenAIUpstreamResponse(account, resp.StatusCode, upstreamMsg, respBody) {
			upstreamDetail := ""
			if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
				maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
				if maxBytes <= 0 {
					maxBytes = 2048
				}
				upstreamDetail = truncateString(string(respBody), maxBytes)
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:            opsUpstreamProxyID(account),
				ProxyName:          opsUpstreamProxyName(account),
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				Kind:               "failover",
				Message:            upstreamMsg,
				Detail:             upstreamDetail,
			})
			shouldDisable := s.handleFailoverSideEffects(ctx, resp, account, respBody, upstreamModel)
			return nil, s.newOpenAIAccountFailoverError(
				account,
				resp.StatusCode,
				resp.Header,
				respBody,
				upstreamMsg,
				shouldDisable,
				!shouldDisable && account.IsPoolMode() && (account.IsPoolModeRetryableStatus(resp.StatusCode) || isOpenAITransientProcessingError(resp.StatusCode, upstreamMsg, respBody)),
			)
		}
		// BPS 的 400/422 是 schema 白名单拒绝（"Invalid request body"），属于
		// 翻译层的确定性客户端错误：归一成 502 会让下游网关把同一坏请求体在
		// 账号池里反复重放（#5479 同类问题），也抹掉定位所需的状态码。直接按
		// 确定性客户端错误回写，不惩罚账号、不 failover。
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity {
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:            opsUpstreamProxyID(account),
				ProxyName:          opsUpstreamProxyName(account),
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				Kind:               "http_error",
				Message:            upstreamMsg,
			})
			MarkResponseCommitted(c)
			writeOpenAIUpstreamClientError(c, resp.StatusCode, respBody, upstreamMsg)
			return nil, fmt.Errorf("basispoints upstream error: %d message=%s", resp.StatusCode, upstreamMsg)
		}
		return s.handleErrorResponse(ctx, resp, c, account, body, requestedModel)
	}

	// 整流读完上游（BPS 协议不做 token 级转发；参考实现的 syntheticStream 模式）。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cfg.MaxResponseBytes+openAIBasisPointsResponseReadLimitPad))
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	if int64(len(raw)) > cfg.MaxResponseBytes {
		return nil, fmt.Errorf("basispoints upstream response exceeds %d bytes", cfg.MaxResponseBytes)
	}

	var responseObj map[string]any
	if isEventStreamResponse(resp.Header) || bodyHasSSEFraming(raw) {
		responseObj, err = basispoints.ParseFinalStreamResponse(raw)
		if err != nil {
			return s.handleBasisPointsStreamFailure(ctx, c, account, resp, err, body, requestedModel, upstreamModel)
		}
	} else {
		responseObj, err = decodeBasisPointsObject(raw)
		if err != nil {
			return nil, fmt.Errorf("basispoints upstream returned invalid JSON")
		}
	}

	upstreamResponseModel := strings.TrimSpace(gjson.GetBytes(mustMarshalBasisPoints(responseObj), "model").String())
	transformed, responseMap, _, err := basispoints.TransformResponseBody(mustMarshalBasisPoints(responseObj), source)
	if err != nil {
		return nil, err
	}
	// 回写客户端请求的模型名：上游恒报 upstream_model，客户端只认自己的 alias。
	if responseMap != nil && requestedModel != "" && upstreamModel != requestedModel {
		responseMap["model"] = requestedModel
		if reEncoded, marshalErr := marshalOpenAIUpstreamJSON(responseMap); marshalErr == nil {
			transformed = reEncoded
		}
	}

	usage, _ := extractOpenAIUsageFromJSONBytes(transformed)
	responseID := strings.TrimSpace(gjson.GetBytes(transformed, "id").String())
	s.bindHTTPResponseAccount(ctx, c, account, responseID)
	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	duration := time.Since(startTime)
	firstTokenMs := int(duration.Milliseconds())
	if reqStream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		if _, writeErr := c.Writer.Write(basispoints.SyntheticStream(responseMap)); writeErr != nil {
			return nil, writeErr
		}
		c.Writer.Flush()
	} else {
		c.Data(http.StatusOK, "application/json", transformed)
	}

	return &OpenAIForwardResult{
		RequestID:             resp.Header.Get("x-request-id"),
		UpstreamHeaders:       resp.Header,
		ResponseID:            responseID,
		Usage:                 usage,
		Model:                 requestedModel,
		UpstreamModel:         upstreamModel,
		UpstreamResponseModel: upstreamResponseModel,
		Stream:                reqStream,
		Duration:              duration,
		FirstTokenMs:          &firstTokenMs,
		UpstreamEndpoint:      openAIBasisPointsUpstreamEndpoint,
		UpstreamTerminalEvent: "response.completed",
	}, nil
}

// handleBasisPointsStreamFailure 处理整流后发现的流内终态失败
// （response.failed/error 或无 terminal response）。语义上等同上游错误响应：
// 可 failover 的失败按 failover 处理，否则把错误体照常写给客户端。
func (s *OpenAIGatewayService) handleBasisPointsStreamFailure(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	resp *http.Response,
	streamErr error,
	requestBody []byte,
	requestedModel, upstreamModel string,
) (*OpenAIForwardResult, error) {
	code := ""
	message := strings.TrimSpace(streamErr.Error())
	if failure, ok := streamErr.(*basispoints.StreamFailure); ok {
		code = failure.Code
		if failure.Message != "" {
			message = failure.Message
		}
	}
	message = sanitizeUpstreamErrorMessage(message)
	failBody, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "server_error",
			"code":    code,
			"message": message,
		},
	})
	failResp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     resp.Header.Clone(),
		Body:       io.NopCloser(bytes.NewReader(failBody)),
	}
	if s.shouldFailoverOpenAIUpstreamResponse(account, failResp.StatusCode, message, failBody) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID:            opsUpstreamProxyID(account),
			ProxyName:          opsUpstreamProxyName(account),
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "failover",
			Message:            message,
		})
		shouldDisable := s.handleFailoverSideEffects(ctx, failResp, account, failBody, upstreamModel)
		return nil, s.newOpenAIAccountFailoverError(
			account,
			failResp.StatusCode,
			resp.Header,
			failBody,
			message,
			shouldDisable,
			!shouldDisable && account.IsPoolMode() && account.IsPoolModeRetryableStatus(failResp.StatusCode),
		)
	}
	return s.handleErrorResponse(ctx, failResp, c, account, requestBody, requestedModel)
}

func decodeBasisPointsObject(raw []byte) (map[string]any, error) {
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	return object, nil
}

func mustMarshalBasisPoints(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}
