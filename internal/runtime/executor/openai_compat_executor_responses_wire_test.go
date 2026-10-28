package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const responsesWireSSE = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"r1","status":"in_progress","output":[]}}` + "\n\n" +
	"event: response.reasoning_summary_text.delta\n" +
	`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"We need answer."}` + "\n\n" +
	"event: response.output_text.delta\n" +
	`data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"391"}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":7,"output_tokens_details":{"reasoning_tokens":5},"total_tokens":18}}}` + "\n\n" +
	"data: [DONE]\n\n"

func newResponsesWireTestExecutor(t *testing.T, wireAPI string) (*OpenAICompatExecutor, *cliproxyauth.Auth, *string, *[]byte) {
	t.Helper()
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		if r.URL.Path == "/v1/responses" {
			if gjson.GetBytes(gotBody, "stream").Bool() {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(responsesWireSSE))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"r1","object":"response","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`))
			return
		}
		if gjson.GetBytes(gotBody, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)
	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{Name: "od", WireAPI: wireAPI}},
	})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":     server.URL + "/v1",
			"api_key":      "test",
			"compat_name":  "od",
			"provider_key": "od",
		},
	}
	return executor, auth, &gotPath, &gotBody
}

const responsesWireRequest = `{"model":"ds-alias","stream":true,"reasoning":{"effort":"high"},"input":[{"role":"developer","content":"You are an agent."},{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`

func TestOpenAICompatExecutorResponsesWireStreamsNatively(t *testing.T) {
	executor, auth, gotPath, gotBody := newResponsesWireTestExecutor(t, "responses")
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4.1-flash",
		Payload: []byte(responsesWireRequest),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var out bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		out.Write(chunk.Payload)
		out.WriteByte('\n')
	}
	if *gotPath != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", *gotPath)
	}
	if got := gjson.GetBytes(*gotBody, "input.0.role").String(); got != "developer" {
		t.Fatalf("input[0].role = %q, want developer: %s", got, *gotBody)
	}
	if gjson.GetBytes(*gotBody, "messages").Exists() || gjson.GetBytes(*gotBody, "stream_options").Exists() {
		t.Fatalf("chat fields leaked into responses body: %s", *gotBody)
	}
	if got := gjson.GetBytes(*gotBody, "model").String(); got != "deepseek-v4.1-flash" {
		t.Fatalf("model = %q, want upstream name: %s", got, *gotBody)
	}
	for _, want := range []string{"response.reasoning_summary_text.delta", "We need answer.", "response.completed"} {
		if !bytes.Contains(out.Bytes(), []byte(want)) {
			t.Fatalf("stream output missing %q:\n%s", want, out.String())
		}
	}
	if bytes.Contains(out.Bytes(), []byte("[DONE]")) {
		t.Fatalf("stream should end at response.completed:\n%s", out.String())
	}
}

func TestOpenAICompatExecutorResponsesWireNonStream(t *testing.T) {
	executor, auth, gotPath, _ := newResponsesWireTestExecutor(t, "responses")
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4.1-flash",
		Payload: []byte(`{"model":"ds-alias","input":"hi"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if *gotPath != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", *gotPath)
	}
	if got := gjson.GetBytes(resp.Payload, "object").String(); got != "response" {
		t.Fatalf("payload = %s", resp.Payload)
	}
}

func TestOpenAICompatExecutorResponsesWireKeepsChatForOtherCases(t *testing.T) {
	cases := []struct {
		name    string
		wireAPI string
		source  string
		payload string
	}{
		{"responses source without wire-api", "", "openai-response", responsesWireRequest},
		{"chat source with wire-api", "responses", "openai", `{"model":"ds-alias","stream":true,"messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor, auth, gotPath, gotBody := newResponsesWireTestExecutor(t, tc.wireAPI)
			result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "deepseek-v4.1-flash",
				Payload: []byte(tc.payload),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(tc.source), Stream: true})
			if err != nil {
				t.Fatalf("ExecuteStream error: %v", err)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream error: %v", chunk.Err)
				}
			}
			if *gotPath != "/v1/chat/completions" {
				t.Fatalf("path = %q, want /v1/chat/completions", *gotPath)
			}
			if got := gjson.GetBytes(*gotBody, "messages.0.role").String(); got != "system" {
				t.Fatalf("messages[0].role = %q, want system: %s", got, *gotBody)
			}
		})
	}
}
