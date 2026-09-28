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
