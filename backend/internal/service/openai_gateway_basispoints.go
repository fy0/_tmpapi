package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
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
//
// 本实现逐行对齐已实测的 bps_proxy.py 参考实现（含 additional_tools 目录、
// JSON catalog 文案、消息 part 归一化、data: 图片上传 /attachments 改写
// file_id、流式请求先行提交 200+SSE 后按心跳保活等）。
const (
	extraKeyOpenAIBasisPoints             = "openai_basispoints"
	extraKeyOpenAIBasisPointsModel        = "openai_basispoints_model"
	extraKeyOpenAIBasisPointsURL          = "openai_basispoints_url"
	extraKeyOpenAIBasisPointsTimezone     = "openai_basispoints_timezone"
	extraKeyOpenAIBasisPointsToolsVerID   = "openai_basispoints_tools_version_id"
	extraKeyOpenAIBasisPointsTimeoutSecs  = "openai_basispoints_timeout_seconds"
	extraKeyOpenAIBasisPointsMaxInputTok  = "openai_basispoints_max_input_tokens"
	openAIBasisPointsUpstreamEndpoint     = "/basispoints/api/responses"
	openAIBasisPointsResponseReadLimitPad = 1 << 10
	openAIBasisPointsKeepaliveInterval    = 20 * time.Second
	openAIBasisPointsUploadTimeout        = 120 * time.Second
	openAIBasisPointsUploadMaxResp        = 1 << 20
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
// 参考实现把客户端 model 原样透传上游（仅剥 -excel 后缀），cfg.UpstreamModel
// 只在客户端没带 model 时兜底：openai_basispoints_model > model_mapping >
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
	if toks := openAIBasisPointsExtraInt64(a, extraKeyOpenAIBasisPointsMaxInputTok); toks > 0 {
		cfg.MaxInputTokens = int(toks)
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

// resolveBasisPointsCredentialAccount 回源到承载凭据的母账号（影子账号凭据
// 透传父账号）；非影子或解析失败时返回自身。
func (s *OpenAIGatewayService) resolveBasisPointsCredentialAccount(ctx context.Context, account *Account) *Account {
	if account != nil && account.IsShadow() && s.accountRepo != nil {
		if resolved, err := resolveCredentialAccount(ctx, s.accountRepo, account); err == nil && resolved != nil {
			return resolved
		}
	}
	return account
}

// basispointsStateFor 返回按凭据账号隔离的运行态（native call 回放缓存 +
// 附件 digest→file_id 缓存）。call_id 与 file_id 的命名空间都在上游
// ChatGPT 账号维度上，影子与母共享同一份缓存。
func (s *OpenAIGatewayService) basispointsStateFor(account *Account) *basispoints.State {
	key := int64(0)
	if account != nil {
		key = account.ID
	}
	if v, ok := s.basispointsStates.Load(key); ok {
		if st, ok := v.(*basispoints.State); ok {
			return st
		}
	}
	st := basispoints.NewState()
	actual, _ := s.basispointsStates.LoadOrStore(key, st)
	if typed, ok := actual.(*basispoints.State); ok {
		return typed
	}
	return st
}

// resolveBasisPointsAccountID 取 ChatGPT account id：优先凭据字段，缺失时
// 从 access_token JWT claim 解析。
func basisPointsAccountID(credAccount *Account, accessToken string) string {
	if credAccount != nil {
		for _, key := range []string{"chatgpt_account_id", "account_id"} {
			if v := strings.TrimSpace(credAccount.GetCredential(key)); v != "" {
				return v
			}
		}
	}
	return basispoints.AccountIDFromToken(accessToken)
}

type basisPointsUpstreamResult struct {
	status int
	header http.Header
	raw    []byte
	err    error
}

// uploadBasisPointsAttachment 对齐参考实现的 Upload 行为：POST
// {responses_url 目录}/attachments 的 multipart 表单 file 字段，返回
// openai_file_id；digest 命中 State 缓存时直接返回。
func (s *OpenAIGatewayService) uploadBasisPointsAttachment(
	ctx context.Context,
	account *Account,
	proxyURL string,
	cfg basispoints.Config,
	token, accountID string,
	st *basispoints.State,
	mediaType string,
	data []byte,
) (string, error) {
	digest := sha256.Sum256(data)
	digestHex := hex.EncodeToString(digest[:])
	if fileID := st.CachedAttachment(digestHex); fileID != "" {
		return fileID, nil
	}

	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	part, err := writer.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="file"; filename="` + basispoints.AttachmentFileName(digestHex, mediaType) + `"`},
		"Content-Type":        {mediaType},
	})
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	uploadCtx, cancel := context.WithTimeout(ctx, openAIBasisPointsUploadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(uploadCtx, http.MethodPost, basispoints.AttachmentsURL(cfg.ResponsesURL), &payload)
	if err != nil {
		return "", err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	headers := basispoints.AuthHeaders(token, accountID, false, cfg)
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")

	resp, err := s.doOpenAIUpstream(req, proxyURL, account)
	if err != nil {
		return "", fmt.Errorf("attachment upload failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, openAIBasisPointsUploadMaxResp))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateString(string(raw), 200))
	}
	fileID := strings.TrimSpace(gjson.GetBytes(raw, "openai_file_id").String())
	if fileID == "" {
		return "", fmt.Errorf("no file id came back")
	}
	st.CacheAttachment(digestHex, fileID)
	return fileID, nil
}

// writeBasisPointsStreamFailure 在已经向下游提交 200+SSE 头之后，用
// response.failed 事件汇报错误（对齐参考实现的 _send_sse_failed；字段集对齐
// handler/writeResponsesFailedSSE——sequence_number 与 created_at 是
// grok-build 等严格客户端的必填字段）。
func writeBasisPointsStreamFailure(c *gin.Context, model, code, message string) {
	var randBytes [16]byte
	_, _ = rand.Read(randBytes[:])
	failed := map[string]any{
		"type":            "response.failed",
		"sequence_number": 0,
		"response": map[string]any{
			"id":         "resp_" + hex.EncodeToString(randBytes[:]),
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "failed",
			"error":      map[string]any{"code": code, "message": message},
			"model":      model,
			"output":     []any{},
		},
	}
	body := "event: response.failed\ndata: " + string(mustMarshalBasisPoints(failed)) + "\n\ndata: [DONE]\n\n"
	_, _ = c.Writer.WriteString(body)
	c.Writer.Flush()
}

// forwardOpenAIBasisPoints 是 /v1/responses 在 Basispoints 通道上的转发实现：
// 客户端 Responses body → 严格白名单 schema（tools→developer 目录、
// instructions→developer、reasoning→reasoning_effort、metadata 重建、
// data: 图片上传改写 file_id）→ Excel 画像头 → 整流上游 SSE →
// run_officejs 回译 → 合成下游响应。流式请求按参考实现先行提交 SSE 头并以
// keepalive 注释保活，客户端断开即中止上游。
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

	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	credAccount := s.resolveBasisPointsCredentialAccount(ctx, account)
	accountID := basisPointsAccountID(credAccount, token)
	if accountID == "" {
		return nil, fmt.Errorf("basispoints: chatgpt account id missing in credentials and access token claims")
	}
	st := s.basispointsStateFor(credAccount)

	upstreamBody, err := basispoints.PrepareResponsesBody(source, cfg, st)
	if err != nil {
		if errors.Is(err, basispoints.ErrInvalidToolEnvelope) {
			message := sanitizeUpstreamErrorMessage(err.Error())
			body := map[string]any{"error": map[string]any{
				"type": "invalid_tool_envelope", "code": "invalid_tool_envelope", "message": message,
			}}
			MarkResponseCommitted(c)
			c.JSON(http.StatusBadRequest, body)
		}
		return nil, fmt.Errorf("prepare basispoints request body: %w", err)
	}
	// 实际上送的 model 以翻译结果为准（客户端 model 剥 -excel 后透传，
	// 空时才落到 cfg.UpstreamModel 兜底）。
	if sent, ok := upstreamBody["model"].(string); ok && strings.TrimSpace(sent) != "" {
		upstreamModel = strings.TrimSpace(sent)
		SetOpsUpstreamModel(c, upstreamModel)
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	// data: 图片先上传到 /attachments 再改写为 file_id（对齐参考实现的
	// rewrite_images 阶段——在 input 翻译之后、发上游之前）。
	uploadCtx, releaseUploadCtx := detachUpstreamContext(ctx)
	defer releaseUploadCtx()
	imgStats := &basispoints.ImageStats{}
	uploader := func(mediaType string, data []byte) (string, error) {
		return s.uploadBasisPointsAttachment(uploadCtx, account, proxyURL, cfg, token, accountID, st, mediaType, data)
	}
	upstreamBody["input"] = basispoints.RewriteImages(upstreamBody["input"], uploader, imgStats)

	// 本地超限拒绝（对齐参考实现：rewrite_images 之后、发上游之前以
	// bytes/3 估算 input token）。BPS 上游的隐性窗口约 272k，超巨请求
	// 发过去只会拿 422/截断，还白烧一次上游配额——本地 400 更省。
	estTokens := basispoints.EstimateInputTokens(upstreamBody["input"])
	if estTokens > cfg.MaxInputTokens {
		msg := fmt.Sprintf("estimated input tokens %d exceed limit %d", estTokens, cfg.MaxInputTokens)
		errBody, _ := json.Marshal(map[string]any{
			"error": map[string]any{"type": "input_too_large", "code": "input_too_large", "message": msg},
		})
		MarkResponseCommitted(c)
		writeOpenAIUpstreamClientError(c, http.StatusBadRequest, errBody, msg)
		return nil, fmt.Errorf("basispoints %s", msg)
	}

	payload, err := marshalOpenAIUpstreamJSON(upstreamBody)
	if err != nil {
		return nil, fmt.Errorf("serialize basispoints request body: %w", err)
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()
	reqCtx, cancelUpstream := context.WithCancel(upstreamCtx)
	if cfg.TimeoutSeconds > 0 {
		var timeoutCancel context.CancelFunc
		reqCtx, timeoutCancel = context.WithTimeout(reqCtx, time.Duration(cfg.TimeoutSeconds)*time.Second)
		defer timeoutCancel()
	}
	defer cancelUpstream()
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

	// 流式客户端启用 SSE 注释心跳（复用 compact keepalive 设施）：上游生成动辄
	// 数分钟且期间零字节，经反向代理时下游空闲会触发读超时、客户端重试复制上游
	// 请求。首拍延迟一个 interval，此前的硬错误仍走 JSON+状态码/failover 链路；
	// 首拍之后 200 固化，错误降级为 response.failed 流内终止事件（#3887 语义）。
	// 参考实现是先行提交 200，这里是延迟提交——严格更优且语义等价。
	var stopKeepalive func()
	if reqStream {
		stopKeepalive = startOpenAISSEKeepalive(c, openAIBasisPointsKeepaliveInterval)
	}
	stopStreamKeepalive := func() bool {
		if stopKeepalive == nil {
			return false
		}
		committed := StopOpenAICompactSSEKeepaliveCommitted(c)
		stopKeepalive()
		return committed
	}

	resultCh := make(chan *basisPointsUpstreamResult, 1)
	go func() {
		resp, doErr := s.doOpenAIUpstream(upstreamReq, proxyURL, account)
		if doErr != nil {
			resultCh <- &basisPointsUpstreamResult{err: doErr}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		result := &basisPointsUpstreamResult{status: resp.StatusCode, header: resp.Header.Clone()}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, cfg.MaxResponseBytes+openAIBasisPointsResponseReadLimitPad))
		if readErr != nil {
			result.err = readErr
		} else if int64(len(raw)) > cfg.MaxResponseBytes {
			result.err = fmt.Errorf("basispoints upstream response exceeds %d bytes", cfg.MaxResponseBytes)
		} else {
			result.raw = raw
		}
		resultCh <- result
	}()

	// 等待上游期间客户端断开则取消上游请求，不再白烧上游额度（对齐参考实现的
	// poll+beat 断线检测；这里直接由 request ctx 驱动，比 5s 轮询更快）。
	var clientDone <-chan struct{}
	if ctx != nil {
		clientDone = ctx.Done()
	}
	var upstream *basisPointsUpstreamResult
	select {
	case upstream = <-resultCh:
	case <-clientDone:
		cancelUpstream()
		<-resultCh
		stopStreamKeepalive()
		return nil, fmt.Errorf("basispoints: client disconnected: %w", ctx.Err())
	}
	streamCommitted := stopStreamKeepalive()

	if upstream.err != nil {
		if streamCommitted {
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:   opsUpstreamProxyID(account),
				ProxyName: opsUpstreamProxyName(account),
				Platform:  account.Platform,
				AccountID: account.ID,
				UpstreamRequestID: func() string {
					if upstream.header != nil {
						return upstream.header.Get("x-request-id")
					}
					return ""
				}(),
				AccountName: account.Name,
				Kind:        "request_error",
				Message:     sanitizeUpstreamErrorMessage(upstream.err.Error()),
			})
			writeBasisPointsStreamFailure(c, upstreamModel, "upstream_error", sanitizeUpstreamErrorMessage(upstream.err.Error()))
			return nil, fmt.Errorf("basispoints upstream request failed: %w", upstream.err)
		}
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, upstream.err, false)
	}

	respHeader := upstream.header
	if respHeader == nil {
		respHeader = http.Header{}
	}
	if upstream.status >= 400 {
		upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(upstream.raw)))
		if streamCommitted {
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:            opsUpstreamProxyID(account),
				ProxyName:          opsUpstreamProxyName(account),
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: upstream.status,
				UpstreamRequestID:  respHeader.Get("x-request-id"),
				Kind:               "http_error",
				Message:            upstreamMsg,
			})
			writeBasisPointsStreamFailure(c, upstreamModel, fmt.Sprintf("upstream_http_%d", upstream.status), truncateString(string(upstream.raw), 500))
			return nil, fmt.Errorf("basispoints upstream error: %d message=%s", upstream.status, upstreamMsg)
		}
		if s.shouldFailoverOpenAIUpstreamResponse(account, upstream.status, upstreamMsg, upstream.raw) {
			upstreamDetail := ""
			if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
				maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
				if maxBytes <= 0 {
					maxBytes = 2048
				}
				upstreamDetail = truncateString(string(upstream.raw), maxBytes)
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:            opsUpstreamProxyID(account),
				ProxyName:          opsUpstreamProxyName(account),
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: upstream.status,
				UpstreamRequestID:  respHeader.Get("x-request-id"),
				Kind:               "failover",
				Message:            upstreamMsg,
				Detail:             upstreamDetail,
			})
			failResp := &http.Response{StatusCode: upstream.status, Header: respHeader, Body: io.NopCloser(bytes.NewReader(upstream.raw))}
			shouldDisable := s.handleFailoverSideEffects(ctx, failResp, account, upstream.raw, upstreamModel)
			return nil, s.newOpenAIAccountFailoverError(
				account,
				upstream.status,
				respHeader,
				upstream.raw,
				upstreamMsg,
				shouldDisable,
				!shouldDisable && account.IsPoolMode() && (account.IsPoolModeRetryableStatus(upstream.status) || isOpenAITransientProcessingError(upstream.status, upstreamMsg, upstream.raw)),
			)
		}
		// BPS 的 400/422 是 schema 白名单拒绝（"Invalid request body"），属于
		// 翻译层的确定性客户端错误：归一成 502 会让下游网关把同一坏请求体在
		// 账号池里反复重放（#5479 同类问题），也抹掉定位所需的状态码。直接按
		// 确定性客户端错误回写，不惩罚账号、不 failover。
		if upstream.status == http.StatusBadRequest || upstream.status == http.StatusUnprocessableEntity {
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:            opsUpstreamProxyID(account),
				ProxyName:          opsUpstreamProxyName(account),
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: upstream.status,
				UpstreamRequestID:  respHeader.Get("x-request-id"),
				Kind:               "http_error",
				Message:            upstreamMsg,
			})
			MarkResponseCommitted(c)
			writeOpenAIUpstreamClientError(c, upstream.status, upstream.raw, upstreamMsg)
			return nil, fmt.Errorf("basispoints upstream error: %d message=%s", upstream.status, upstreamMsg)
		}
		failResp := &http.Response{StatusCode: upstream.status, Header: respHeader, Body: io.NopCloser(bytes.NewReader(upstream.raw))}
		return s.handleErrorResponse(ctx, failResp, c, account, body, requestedModel)
	}

	// 整流读完上游（BPS 协议不做 token 级转发；参考实现的 syntheticStream 模式）。
	raw := upstream.raw
	var responseObj map[string]any
	if isEventStreamResponse(respHeader) || bodyHasSSEFraming(raw) {
		responseObj, err = basispoints.ParseFinalStreamResponse(raw)
		if err != nil {
			if streamCommitted {
				writeBasisPointsStreamFailure(c, upstreamModel, "invalid_upstream_response", sanitizeUpstreamErrorMessage(err.Error()))
				return nil, fmt.Errorf("basispoints upstream stream failed: %w", err)
			}
			return s.handleBasisPointsStreamFailure(ctx, c, account, respHeader, err, body, requestedModel, upstreamModel)
		}
	} else {
		responseObj, err = decodeBasisPointsObject(raw)
		if err != nil {
			if streamCommitted {
				writeBasisPointsStreamFailure(c, upstreamModel, "invalid_upstream_response", "Basis Points returned invalid JSON")
				return nil, fmt.Errorf("basispoints upstream returned invalid JSON")
			}
			return nil, fmt.Errorf("basispoints upstream returned invalid JSON")
		}
	}

	upstreamResponseModel := strings.TrimSpace(gjson.GetBytes(mustMarshalBasisPoints(responseObj), "model").String())
	transformed, responseMap, _, err := basispoints.TransformResponseBody(mustMarshalBasisPoints(responseObj), source, st)
	if err != nil {
		if errors.Is(err, basispoints.ErrInvalidToolEnvelope) {
			message := sanitizeUpstreamErrorMessage(err.Error())
			if streamCommitted {
				writeBasisPointsStreamFailure(c, upstreamModel, "invalid_tool_envelope", message)
				return nil, fmt.Errorf("basispoints response tool envelope: %w", err)
			}
			MarkResponseCommitted(c)
			c.JSON(http.StatusBadGateway, map[string]any{"error": map[string]any{
				"type": "invalid_tool_envelope", "code": "invalid_tool_envelope", "message": message,
			}})
			return nil, fmt.Errorf("basispoints response tool envelope: %w", err)
		}
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
	if !streamCommitted {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), respHeader, s.responseHeaderFilter)
	}

	duration := time.Since(startTime)
	firstTokenMs := int(duration.Milliseconds())
	if reqStream {
		if !streamCommitted {
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")
			MarkResponseCommitted(c)
		}
		if _, writeErr := c.Writer.Write(basispoints.SyntheticStream(responseMap)); writeErr != nil {
			return nil, writeErr
		}
		c.Writer.Flush()
	} else {
		c.Data(http.StatusOK, "application/json", transformed)
	}

	return &OpenAIForwardResult{
		RequestID:             respHeader.Get("x-request-id"),
		UpstreamHeaders:       respHeader,
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
	respHeader http.Header,
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
		Header:     respHeader.Clone(),
		Body:       io.NopCloser(bytes.NewReader(failBody)),
	}
	if s.shouldFailoverOpenAIUpstreamResponse(account, failResp.StatusCode, message, failBody) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID:            opsUpstreamProxyID(account),
			ProxyName:          opsUpstreamProxyName(account),
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: failResp.StatusCode,
			UpstreamRequestID:  respHeader.Get("x-request-id"),
			Kind:               "failover",
			Message:            message,
		})
		shouldDisable := s.handleFailoverSideEffects(ctx, failResp, account, failBody, upstreamModel)
		return nil, s.newOpenAIAccountFailoverError(
			account,
			failResp.StatusCode,
			respHeader,
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
