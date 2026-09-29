package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const basisPointsURL = "https://bps.openai.com/basispoints/api/responses"

type basisPointsSetup struct {
	upstream *httpUpstreamRecorder
	svc      *OpenAIGatewayService
	c        *gin.Context
	rec      *httptest.ResponseRecorder
	account  *Account
}

// newBasisPointsSetup builds an OpenAI OAuth account with the Basispoints
// channel enabled via accounts.extra.openai_basispoints.
func newBasisPointsSetup(t *testing.T, extra map[string]any) *basisPointsSetup {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Gateway: config.GatewayConfig{}},
		httpUpstream: upstream,
	}
	merged := map[string]any{"openai_basispoints": true}
	for key, value := range extra {
		merged[key] = value
	}
	account := &Account{
		ID:          901,
		Name:        "oauth-bps",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
		Extra:          merged,
		Status:         StatusActive,
		Schedulable:    true,
		RateMultiplier: f64p(1),
	}
	return &basisPointsSetup{upstream: upstream, svc: svc, c: c, rec: rec, account: account}
}

func bpsCompletedSSE(inner string) string {
	return "data: {\"type\":\"response.completed\",\"response\":" + inner + "}\n\ndata: [DONE]\n\n"
}

func bpsUpstreamSSEResponse(inner string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_bps"}},
		Body:       io.NopCloser(strings.NewReader(bpsCompletedSSE(inner))),
	}
}

// TestForward_BasisPoints_Stream_RequestShape asserts the upstream request is
// rewritten to the strict Basispoints schema plus the Excel client header
// profile, and the buffered upstream SSE is re-synthesized downstream.
func TestForward_BasisPoints_Stream_RequestShape(t *testing.T) {
	s := newBasisPointsSetup(t, map[string]any{
		"openai_basispoints_timezone": "America/New_York",
	})
	inner := `{"id":"resp_bps","model":"gpt-6-astra","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"output_tokens_details":{"reasoning_tokens":5}}}`
	s.upstream.resp = bpsUpstreamSSEResponse(inner)
	body := []byte(`{"model":"gpt-6-astra","stream":true,"instructions":"be terse","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"reasoning":{"effort":"high"}}`)

	result, err := s.svc.Forward(context.Background(), s.c, s.account, body)
	require.NoError(t, err)
	require.NotNil(t, result)

	req := s.upstream.lastReq
	require.NotNil(t, req)
	require.Equal(t, basisPointsURL, req.URL.String())
	require.Equal(t, "Bearer oauth-token", req.Header.Get("Authorization"))
	require.Equal(t, "chatgpt-acc", req.Header.Get("Chatgpt-Account-Id"))
	require.Equal(t, "chatgpt-acc", req.Header.Get("X-Openai-Account-Id"))
	require.Equal(t, "chatgpt", req.Header.Get("X-Basispoints-Auth-Mode"))
	require.Equal(t, "excel", req.Header.Get("X-Openai-Internal-Basispoints-Client-Agent-Profile"))
	require.Equal(t, "excel", req.Header.Get("X-Openai-Internal-Basispoints-Client-Platform"))
	require.Equal(t, "basispoints-excel-plugin", req.Header.Get("X-Openai-Internal-Basispoints-Client-Product"))
	require.Equal(t, "browser:chrome", req.Header.Get("X-Stainless-Runtime"))
	require.Equal(t, "6.31.0", req.Header.Get("X-Stainless-Package-Version"))
	require.Equal(t, "text/event-stream", req.Header.Get("Accept"))
	require.Equal(t, "America/New_York", req.Header.Get("X-Oai-Timezone"))
	// Codex 身份头不得泄露到 Basispoints 上游。
	require.Empty(t, req.Header.Get("X-Codex-Turn-State"))
	require.Empty(t, req.Header.Get("Originator"))

	up := gjson.ParseBytes(s.upstream.lastBody)
	require.Equal(t, "gpt-6-astra", up.Get("model").String())
	require.Equal(t, "explicit", up.Get("model_selection").String())
	require.True(t, up.Get("stream").Bool())
	require.False(t, up.Get("store").Bool())
	require.Equal(t, "high", up.Get("reasoning_effort").String())
	require.Equal(t, "compaction", up.Get("context_management.0.type").String())
	// instructions → developer message；客户端 instructions 原文必须可见。
	input := up.Get("input").Array()
	require.GreaterOrEqual(t, len(input), 2)
	require.Equal(t, "developer", input[0].Get("role").String())
	require.Equal(t, "be terse", input[0].Get("content.0.text").String())
	require.Equal(t, "user", input[len(input)-1].Get("role").String())
	// metadata 严格白名单：只有 task_id/turn_id/agent_iteration。
	meta := up.Get("metadata")
	require.True(t, meta.Get("task_id").Exists())
	require.True(t, meta.Get("turn_id").Exists())
	require.Equal(t, "1", meta.Get("agent_iteration").String())
	for key := range meta.Map() {
		require.Contains(t, []string{"task_id", "turn_id", "agent_iteration", "bps_tools_version_id"}, key)
	}
	require.False(t, up.Get("tools").Exists())
	require.False(t, up.Get("instructions").Exists())
	require.False(t, up.Get("reasoning").Exists())

	downstream := s.rec.Body.String()
	require.Contains(t, downstream, "response.completed")
	require.Contains(t, downstream, `"text":"hi"`)
	require.Equal(t, 11, result.Usage.InputTokens)
	require.Equal(t, 7, result.Usage.OutputTokens)
	require.Equal(t, "gpt-6-astra", result.Model)
	require.Equal(t, "gpt-6-astra", result.UpstreamModel)
	require.True(t, result.Stream)
	require.Equal(t, "/basispoints/api/responses", result.UpstreamEndpoint)
}

// TestForward_BasisPoints_NonStream covers a stream=false client request: the
// upstream still answers SSE; the gateway aggregates to a JSON response.
func TestForward_BasisPoints_NonStream(t *testing.T) {
	s := newBasisPointsSetup(t, nil)
	inner := `{"id":"resp_bps2","model":"gpt-6-astra","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
	s.upstream.resp = bpsUpstreamSSEResponse(inner)
	body := []byte(`{"model":"gpt-6-astra","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)

	result, err := s.svc.Forward(context.Background(), s.c, s.account, body)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.False(t, gjson.GetBytes(s.upstream.lastBody, "stream").Bool())
	require.Equal(t, "application/json", s.upstream.lastReq.Header.Get("Accept"))

	downstream := s.rec.Body.String()
	require.True(t, gjson.Valid(downstream), "downstream must be aggregated JSON, got: %s", downstream)
	require.Equal(t, "resp_bps2", gjson.Get(downstream, "id").String())
	require.Equal(t, "done", gjson.Get(downstream, "output.0.content.0.text").String())
	require.Equal(t, 3, result.Usage.InputTokens)
	require.False(t, result.Stream)
}

// TestForward_BasisPoints_ToolRelay exercises the run_officejs transport:
// client tools are stripped into a developer catalog; a single upstream
// run_officejs call is rewritten to the real client tool call; the next turn
// replays the cached native item plus a normalized function_call_output while
// turn_id stays constant and agent_iteration increments.
func TestForward_BasisPoints_ToolRelay(t *testing.T) {
	s := newBasisPointsSetup(t, nil)

	// envelope 内层 code 对齐参考实现形态：{"name":..., "arguments":{...}}。
	runOfficeCall := `{"type":"function_call","id":"fc_native_1","call_id":"call_abc","name":"run_officejs","arguments":"{\"summary\":\"run\",\"extended_summary\":\"relay\",\"destructive\":false,\"references\":[],\"code\":\"{\\\"name\\\":\\\"exec_command\\\",\\\"arguments\\\":{\\\"cmd\\\":\\\"pwd\\\"}}\"}","status":"completed"}`
	inner1 := `{"id":"resp_t1","model":"gpt-6-astra","status":"completed","output":[` + runOfficeCall + `],"usage":{"input_tokens":5,"output_tokens":4,"total_tokens":9}}`
	s.upstream.resp = bpsUpstreamSSEResponse(inner1)

	body1 := []byte(`{"model":"gpt-6-astra","stream":true,"prompt_cache_key":"conv-1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"run pwd"}]}],"tools":[{"type":"function","name":"exec_command","description":"run a shell command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}]}`)

	result, err := s.svc.Forward(context.Background(), s.c, s.account, body1)
	require.NoError(t, err)
	require.NotNil(t, result)

	up1 := gjson.ParseBytes(s.upstream.lastBody)
	require.False(t, up1.Get("tools").Exists(), "tools must be stripped for basispoints")
	require.False(t, up1.Get("tool_choice").Exists())
	// 工具目录 developer 消息必须列出 exec_command。
	catalogSeen := false
	for _, item := range up1.Get("input").Array() {
		if item.Get("role").String() == "developer" && strings.Contains(item.Get("content.0.text").String(), "exec_command") {
			catalogSeen = true
		}
	}
	require.True(t, catalogSeen, "developer tool catalog must list exec_command")

	downstream := s.rec.Body.String()
	require.Contains(t, downstream, `"name":"exec_command"`, "run_officejs envelope must be rewritten to the client tool: %s", downstream)
	require.NotContains(t, downstream, `"name":"run_officejs"`)
	turnID1 := up1.Get("metadata.turn_id").String()
	taskID1 := up1.Get("metadata.task_id").String()
	require.NotEmpty(t, turnID1)
	require.Equal(t, "1", up1.Get("metadata.agent_iteration").String())

	// 第二回合：客户端回传工具结果。input 里必须重放原生 run_officejs item
	// （保留模型生成的 summary/references 等字段）+ 归一化 function_call_output。
	s.rec = httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(s.rec)
	c2.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	s.c = c2
	inner2 := `{"id":"resp_t2","model":"gpt-6-astra","status":"completed","output":[{"type":"message","id":"msg_2","role":"assistant","content":[{"type":"output_text","text":"/work"}]}],"usage":{"input_tokens":6,"output_tokens":2,"total_tokens":8}}`
	s.upstream.resp = bpsUpstreamSSEResponse(inner2)
	body2 := []byte(`{"model":"gpt-6-astra","stream":true,"prompt_cache_key":"conv-1","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"run pwd"}]},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"},` +
		`{"type":"function_call_output","call_id":"call_abc","output":"/work"}` +
		`]}`)

	_, err = s.svc.Forward(context.Background(), s.c, s.account, body2)
	require.NoError(t, err)

	up2 := gjson.ParseBytes(s.upstream.lastBody)
	require.Equal(t, taskID1, up2.Get("metadata.task_id").String(), "task_id must be stable per conversation")
	require.Equal(t, turnID1, up2.Get("metadata.turn_id").String(), "turn_id must stay constant across tool replay")
	require.Equal(t, "2", up2.Get("metadata.agent_iteration").String())
	// 回放的原生 run_officejs item：模型字段原样带回，不换成 exec_command。
	replayed := false
	for _, item := range up2.Get("input").Array() {
		if item.Get("type").String() == "function_call" && item.Get("call_id").String() == "call_abc" {
			require.Equal(t, "run_officejs", item.Get("name").String(), "replayed call must keep the native run_officejs identity")
			require.Contains(t, item.Get("arguments").String(), "extended_summary")
			replayed = true
		}
	}
	require.True(t, replayed, "cached native run_officejs item must be replayed")
	sawNormalizedOutput := false
	for _, item := range up2.Get("input").Array() {
		if item.Get("type").String() == "function_call_output" && item.Get("call_id").String() == "call_abc" {
			sawNormalizedOutput = true
			require.True(t, strings.HasPrefix(item.Get("id").String(), "fc_"), "deterministic uuid5 id: %s", item.Get("id").String())
			require.Equal(t, "/work", item.Get("output").String())
		}
	}
	require.True(t, sawNormalizedOutput, "normalized function_call_output must be replayed next to the native item")
}

// TestForward_BasisPoints_UpstreamError asserts upstream HTTP errors keep the
// regular OpenAI error/failover semantics (422 schema rejection is surfaced
// to the client rather than retried on every account).
func TestForward_BasisPoints_UpstreamError(t *testing.T) {
	s := newBasisPointsSetup(t, nil)
	s.upstream.resp = &http.Response{
		StatusCode: http.StatusUnprocessableEntity,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"422: Invalid request body."}}`)),
	}
	body := []byte(`{"model":"gpt-6-astra","stream":true,"input":"hi"}`)

	_, err := s.svc.Forward(context.Background(), s.c, s.account, body)
	require.Error(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, s.rec.Code)
	require.Contains(t, s.rec.Body.String(), "Invalid request body")
}

// TestForward_BasisPoints_ExposureHardening 覆盖参考实现新增的「非标准暴露
// 面」收敛：客户端自带的 context_management 不透传（恒定 stock 200000）、
// codex 泄露的 token_budget developer 片段被剥除、-bps 模型后缀剥除。
func TestForward_BasisPoints_ExposureHardening(t *testing.T) {
	s := newBasisPointsSetup(t, nil)
	inner := `{"id":"resp_h","model":"gpt-6-astra","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	s.upstream.resp = bpsUpstreamSSEResponse(inner)
	body := []byte(`{"model":"gpt-6-astra-bps","stream":false,"input":[` +
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"<context_window>You have 250000 tokens left in this context window</context_window>"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}` +
		`],"context_management":[{"type":"compaction","compact_threshold":1}]}`)

	_, err := s.svc.Forward(context.Background(), s.c, s.account, body)
	require.NoError(t, err)

	up := gjson.ParseBytes(s.upstream.lastBody)
	require.Equal(t, "gpt-6-astra", up.Get("model").String(), "-bps suffix must be stripped")
	require.Equal(t, int64(200000), up.Get("context_management.0.compact_threshold").Int(),
		"client-supplied context_management must be pinned to the stock value")
	for _, item := range up.Get("input").Array() {
		require.NotContains(t, item.Get("content.0.text").String(), "context_window",
			"token_budget developer fragment must be stripped")
	}
}

// TestForward_BasisPoints_InputTooLarge 覆盖本地超限拒绝：估算 input token
// 超阈值时回 400 input_too_large，且不打上游。
func TestForward_BasisPoints_InputTooLarge(t *testing.T) {
	s := newBasisPointsSetup(t, map[string]any{
		"openai_basispoints_max_input_tokens": 10,
	})
	body := []byte(`{"model":"gpt-6-astra","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` +
		strings.Repeat("x", 4096) + `"}]}]}`)

	_, err := s.svc.Forward(context.Background(), s.c, s.account, body)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, s.rec.Code)
	require.Contains(t, s.rec.Body.String(), "input_too_large")
	require.Nil(t, s.upstream.lastReq, "oversized input must be rejected locally without hitting upstream")
}

func TestForward_BasisPoints_InvalidToolEnvelope(t *testing.T) {
	tool := map[string]any{"type": "function", "name": "bash", "parameters": map[string]any{
		"type": "object", "required": []any{"command"},
		"properties": map[string]any{"command": map[string]any{"type": "string"}},
	}}
	code := `{"name":"bash","arguments":{"command":"echo "hello""}}`
	arguments, err := json.Marshal(map[string]any{"code": code})
	require.NoError(t, err)
	call := map[string]any{"type": "function_call", "id": "fc_bad", "call_id": "call_bad", "name": "run_officejs", "arguments": string(arguments)}
	upstreamBody, err := json.Marshal(map[string]any{
		"id": "resp_bad", "model": "gpt-6-astra", "status": "completed", "output": []any{call},
	})
	require.NoError(t, err)

	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("bad upstream stream=%v", streaming), func(t *testing.T) {
			s := newBasisPointsSetup(t, nil)
			s.upstream.resp = bpsUpstreamSSEResponse(string(upstreamBody))
			body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "stream": streaming, "input": "hi", "tools": []any{tool}})
			require.NoError(t, err)
			_, err = s.svc.Forward(context.Background(), s.c, s.account, body)
			require.Error(t, err)
			require.Equal(t, http.StatusBadGateway, s.rec.Code)
			require.True(t, IsResponseCommitted(s.c))
			require.Equal(t, "invalid_tool_envelope", gjson.Get(s.rec.Body.String(), "error.code").String())
			require.NotContains(t, s.rec.Body.String(), `"name":"run_officejs"`)
		})
	}

	t.Run("bad history", func(t *testing.T) {
		s := newBasisPointsSetup(t, nil)
		body, err := json.Marshal(map[string]any{
			"model": "gpt-6-astra", "stream": false, "tools": []any{tool},
			"input": []any{map[string]any{
				"type": "function_call", "name": "bash", "call_id": "history_bad",
				"arguments": `{"command":"echo "hello""}`,
			}},
		})
		require.NoError(t, err)
		_, err = s.svc.Forward(context.Background(), s.c, s.account, body)
		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, s.rec.Code)
		require.True(t, IsResponseCommitted(s.c))
		require.Equal(t, "invalid_tool_envelope", gjson.Get(s.rec.Body.String(), "error.code").String())
		require.Nil(t, s.upstream.lastReq, "bad input must be rejected before contacting upstream")
	})
}

// TestForward_BasisPoints_CompactionTriggerBypassesBPS：remote compaction v2
// （裸 /responses + input 末尾 compaction_trigger）不能走 BPS——BPS 端点不会
// 产出 compaction output item，触发回合必须回 Codex 通道。
func TestForward_BasisPoints_CompactionTriggerBypassesBPS(t *testing.T) {
	s := newBasisPointsSetup(t, nil)
	inner := `{"id":"resp_cmp","model":"gpt-6-astra","status":"completed","output":[{"type":"compaction","id":"cmp_1","encrypted_content":"abc"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	s.upstream.resp = bpsUpstreamSSEResponse(inner)
	body := []byte(`{"model":"gpt-6-astra","stream":true,"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"compaction_trigger"}` +
		`]}`)

	_, err := s.svc.Forward(context.Background(), s.c, s.account, body)
	require.NoError(t, err)
	require.NotNil(t, s.upstream.lastReq)
	require.Equal(t, "https://chatgpt.com/backend-api/codex/responses", s.upstream.lastReq.URL.String(),
		"compaction trigger requests must stay on the Codex channel")
}

// TestForward_BasisPoints_DisabledKeepsCodex verifies the flag gate: without
// extra.openai_basispoints the same account forwards to chatgpt.com.
func TestForward_BasisPoints_DisabledKeepsCodex(t *testing.T) {
	s := newBasisPointsSetup(t, nil)
	s.account.Extra = map[string]any{}
	inner := `{"id":"resp_codex","model":"gpt-6-astra","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	s.upstream.resp = bpsUpstreamSSEResponse(inner)
	body := []byte(`{"model":"gpt-6-astra","stream":true,"input":"hi"}`)

	_, err := s.svc.Forward(context.Background(), s.c, s.account, body)
	require.NoError(t, err)
	require.Equal(t, "https://chatgpt.com/backend-api/codex/responses", s.upstream.lastReq.URL.String())
}
