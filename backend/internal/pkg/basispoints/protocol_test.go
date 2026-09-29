package basispoints

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func transportTestSource() map[string]any {
	return map[string]any{"tools": []any{
		map[string]any{"type": "function", "name": "bash", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required": []any{"command"},
		}},
		map[string]any{"type": "function", "name": "read", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required": []any{"path"},
		}},
	}}
}

func nativeTestCall(callID, code string) map[string]any {
	return map[string]any{
		"type": "function_call", "id": functionItemID(callID),
		"call_id": callID, "name": transportName,
		"arguments": string(dumps(map[string]any{"code": code})),
	}
}

func TestTransformResponseBodyRepairsInvalidEscape(t *testing.T) {
	// A model-generated code string can contain a single invalid JSON escape.
	code := `{"name":"bash","arguments":{"command":"rg -g '!\.git/**'"}}`
	if json.Valid([]byte(code)) {
		t.Fatal("fixture must contain an invalid JSON escape")
	}
	st := NewState()
	response := map[string]any{"status": "completed", "output": []any{nativeTestCall("call_1", code)}}
	out, _, converted, err := TransformResponseBody(dumps(response), transportTestSource(), st)
	if err != nil || !converted {
		t.Fatalf("expected repaired call, converted=%v err=%v", converted, err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	call := result["output"].([]any)[0].(map[string]any)
	if call["name"] != "bash" || st.NativeCall("call_1") == nil {
		t.Fatalf("call was not converted and cached: %v", call)
	}
	args := parseArguments(call["arguments"])
	if args["command"] != `rg -g '!\.git/**'` {
		t.Fatalf("command changed during repair: %v", args)
	}
}

func TestTransformResponseBodyRejectsInvalidEnvelopes(t *testing.T) {
	cases := map[string]string{
		"quotes":   `{"name":"bash","arguments":{"command":"echo "hello""}}`,
		"trailing": `{"name":"bash","arguments":{"command":"pwd"}} after`,
		"unknown":  `{"name":"missing","arguments":{}}`,
		"schema":   `{"name":"bash","arguments":{}}`,
	}
	for name, code := range cases {
		t.Run(name, func(t *testing.T) {
			st := NewState()
			response := map[string]any{"output": []any{nativeTestCall("call_bad", code)}}
			out, _, converted, err := TransformResponseBody(dumps(response), transportTestSource(), st)
			if !errors.Is(err, ErrInvalidToolEnvelope) || converted || out != nil {
				t.Fatalf("expected invalid envelope, got converted=%v err=%v out=%s", converted, err, out)
			}
			if st.NativeCall("call_bad") != nil {
				t.Fatal("rejected call must not be cached")
			}
		})
	}
}

func TestTransformResponseBodyMultipleCalls(t *testing.T) {
	st := NewState()
	first := nativeTestCall("call_1", `{"name":"bash","arguments":{"command":"pwd"}}`)
	second := nativeTestCall("call_2", `{"name":"read","arguments":{"path":"README.md"}}`)
	response := map[string]any{"output": []any{first, map[string]any{"type": "message", "role": "assistant"}, second}}
	out, _, converted, err := TransformResponseBody(dumps(response), transportTestSource(), st)
	if err != nil || !converted {
		t.Fatalf("parallel calls not converted: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	items := result["output"].([]any)
	if items[0].(map[string]any)["name"] != "bash" || items[2].(map[string]any)["name"] != "read" {
		t.Fatalf("calls changed order: %v", items)
	}
	if st.NativeCall("call_1") == nil || st.NativeCall("call_2") == nil {
		t.Fatal("valid calls were not cached")
	}

	st = NewState()
	response["output"] = []any{first, nativeTestCall("call_bad", `{"name":"bash","arguments":{"command":"x"}} junk`)}
	out, _, _, err = TransformResponseBody(dumps(response), transportTestSource(), st)
	if !errors.Is(err, ErrInvalidToolEnvelope) || out != nil {
		t.Fatalf("invalid batch should be rejected: %v", err)
	}
	if st.NativeCall("call_1") != nil || st.NativeCall("call_bad") != nil {
		t.Fatal("invalid batch must not partially cache calls")
	}
}

func TestPrepareResponsesBodyRejectsInvalidHistory(t *testing.T) {
	source := transportTestSource()
	source["input"] = []any{map[string]any{
		"type": "function_call", "name": "bash", "call_id": "history_1",
		"arguments": `{"command":"rg -g '!\.git/**'"}`,
	}}
	body, err := PrepareResponsesBody(source, DefaultConfig(), NewState())
	if err != nil {
		t.Fatal(err)
	}
	items := body["input"].([]any)
	wrapped := items[len(items)-1].(map[string]any)
	inner := parseArguments(parseArguments(wrapped["arguments"])["code"])
	if got := parseArguments(inner["arguments"])["command"]; got != `rg -g '!\.git/**'` {
		t.Fatalf("history command changed: %v", got)
	}

	source["input"] = []any{map[string]any{
		"type": "function_call", "name": "bash", "call_id": "history_bad",
		"arguments": `{"command":"echo "hello""}`,
	}}
	if _, err := PrepareResponsesBody(source, DefaultConfig(), NewState()); !errors.Is(err, ErrInvalidToolEnvelope) {
		t.Fatalf("bad history was silently replaced with empty arguments: %v", err)
	}
}

func TestTransformResponseBodyWithoutToolCallPassesThrough(t *testing.T) {
	raw := []byte(`{"output":[{"type":"message","role":"assistant","content":[]}]}`)
	out, _, converted, err := TransformResponseBody(raw, transportTestSource(), NewState())
	if err != nil || converted || !strings.EqualFold(string(out), string(raw)) {
		t.Fatalf("plain response changed: converted=%v err=%v", converted, err)
	}
}

// TestTransformResponseBodyCustomToolRetypesItemID：上游 run_officejs 是
// function_call（fc_ id），翻回客户端 custom_tool_call 时 id 必须重冠成
// ctc_——客户端原样回放 id，上游按类型校验前缀，fc_ 会 400 "Expected an ID
// that begins with 'ctc'"。
func TestTransformResponseBodyCustomToolRetypesItemID(t *testing.T) {
	st := NewState()
	source := map[string]any{"tools": []any{
		map[string]any{"type": "custom", "name": "apply_patch",
			"format": map[string]any{"type": "grammar"}},
	}}
	suffix := "0123456789abcdef0123456789abcdef0123456789abcdef"
	call := map[string]any{
		"type": "function_call", "id": "fc_" + suffix,
		"call_id": "call_1", "name": "run_officejs",
		"arguments": string(dumps(map[string]any{
			"code": `{"name":"apply_patch","input":"*** Begin Patch\n*** End Patch"}`,
		})),
	}
	response := map[string]any{"status": "completed", "output": []any{call}}
	out, _, converted, err := TransformResponseBody(dumps(response), source, st)
	if err != nil || !converted {
		t.Fatalf("custom call not converted: converted=%v err=%v", converted, err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	item := result["output"].([]any)[0].(map[string]any)
	if item["type"] != "custom_tool_call" {
		t.Fatalf("expected custom_tool_call, got %v", item["type"])
	}
	if id := stringValue(item["id"]); id != "ctc_"+suffix {
		t.Fatalf("fc_ id must be retyped to ctc_ preserving suffix, got %q", id)
	}
	if stringValue(item["call_id"]) != "call_1" {
		t.Fatalf("call_id changed: %v", item["call_id"])
	}
}

// TestPrepareResponsesBodyRetypesPoisonedCallIDs：存量会话里已被污染的
// custom_tool_call（fc_ id，名字不在本次 catalog、State 未命中）透传前按
// 类型重冠，不再把坏前缀发给上游；function_call 上的 ctc_ id 镜像修复；
// output item 的非 fc 前缀 id（ctco_）剥除，fc 前缀保留。
func TestPrepareResponsesBodyRetypesPoisonedCallIDs(t *testing.T) {
	source := transportTestSource()
	source["input"] = []any{
		map[string]any{
			"type": "custom_tool_call", "id": "fc_poisoned", "call_id": "call_gone",
			"name": "undeclared_tool", "input": "raw",
		},
		map[string]any{
			"type": "function_call", "id": "ctc_swapped", "call_id": "call_gone2",
			"name": "undeclared_fn", "arguments": "{}",
		},
		map[string]any{
			"type": "custom_tool_call_output", "id": "ctco_bad", "call_id": "call_out",
			"output": "done",
		},
		map[string]any{
			"type": "function_call_output", "id": "fco_ok", "call_id": "call_out2",
			"output": "done",
		},
	}
	body, err := PrepareResponsesBody(source, DefaultConfig(), NewState())
	if err != nil {
		t.Fatal(err)
	}
	items := body["input"].([]any)
	ctc := items[len(items)-4].(map[string]any)
	fc := items[len(items)-3].(map[string]any)
	ctco := items[len(items)-2].(map[string]any)
	fco := items[len(items)-1].(map[string]any)
	if got := stringValue(ctc["id"]); got != "ctc_poisoned" {
		t.Fatalf("poisoned custom_tool_call id not healed: %q", got)
	}
	if stringValue(ctc["type"]) != "custom_tool_call" {
		t.Fatalf("type changed: %v", ctc["type"])
	}
	if got := stringValue(fc["id"]); got != "fc_swapped" {
		t.Fatalf("swapped function_call id not healed: %q", got)
	}
	if _, exists := ctco["id"]; exists {
		t.Fatalf("non-fc output id must be stripped: %v", ctco["id"])
	}
	if got := stringValue(fco["id"]); got != "fco_ok" {
		t.Fatalf("fc-prefixed output id must be kept: %q", got)
	}
}
