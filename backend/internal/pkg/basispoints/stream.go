package basispoints

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// StreamFailure describes a terminal failure observed on the upstream SSE
// stream (response.failed / error events).
type StreamFailure struct {
	Type    string
	Code    string
	Message string
}

func (e *StreamFailure) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return "basispoints upstream stream failed: " + e.Type
}

// ParseFinalStreamResponse scans a fully-buffered upstream SSE body and
// returns the terminal response object. It prefers the response.completed /
// response.done payload, then any embedded response whose status is
// "completed". A terminal failure event yields a *StreamFailure; a stream that
// ends without a terminal response yields a generic error.
func ParseFinalStreamResponse(raw []byte) (map[string]any, error) {
	var completed map[string]any
	var failure *StreamFailure

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	var dataLines []string
	flushEvent := func() {
		if len(dataLines) == 0 {
			return
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if payload == "[DONE]" {
			return
		}
		var event map[string]any
		decoder := json.NewDecoder(strings.NewReader(payload))
		decoder.UseNumber()
		if decoder.Decode(&event) != nil || event == nil {
			return
		}
		eventType := stringValue(event["type"])
		response := objectValue(event["response"])
		switch eventType {
		case "response.completed", "response.done":
			if response != nil {
				completed = response
			}
		case "response.failed", "error":
			failure = streamFailureFromEvent(eventType, event, response)
		case "response.incomplete":
			if response != nil {
				completed = response
			}
		default:
			if response != nil && completed == nil &&
				strings.EqualFold(stringValue(response["status"]), "completed") {
				completed = response
			}
		}
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			flushEvent()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flushEvent()

	if completed != nil {
		return completed, nil
	}
	if failure != nil {
		return nil, failure
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read basispoints stream: %w", err)
	}
	return nil, fmt.Errorf("basispoints stream ended without a terminal response")
}

func streamFailureFromEvent(eventType string, event, response map[string]any) *StreamFailure {
	failure := &StreamFailure{Type: eventType}
	for _, container := range []map[string]any{objectValue(event["error"]), objectValue(response["error"]), event} {
		if container == nil {
			continue
		}
		if code := stringValue(container["code"]); code != "" && failure.Code == "" {
			failure.Code = code
		}
		if message := stringValue(container["message"]); message != "" && failure.Message == "" {
			failure.Message = message
		}
	}
	return failure
}
