package executor

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestDevinPrepareHTTPRequest_AgentSDKIdentity(t *testing.T) {
	const identity = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
	const task = "Summarize the supplied webpage."
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			system string
			want   string
			cfg    config.Config
		}{
			{name: "background webfetch", system: fmt.Sprintf(`[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.295; cc_entrypoint=claude-vscode;"},{"type":"text","text":%q}]`, identity)},
			{name: "preserve task", system: fmt.Sprintf(`[{"type":"text","text":%q},{"type":"text","text":%q}]`, identity, task), want: task},
			{name: "string system", system: fmt.Sprintf("%q", identity+"\n"+task), want: task},
			{
				name: "payload override remains final", system: fmt.Sprintf("%q", identity), want: identity,
				cfg: config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
					Models: []config.PayloadModelRule{{Name: "devin/claude-haiku-5-5", Protocol: "devin"}},
					Params: map[string]any{"system_prompt": identity},
				}}}},
			},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				// A quoted identity in webpage/user content is not a system header.
				content := "Page content: " + identity
				payload := []byte(fmt.Sprintf(`{"model":"devin/claude-haiku-5-5","max_tokens":256,"stream":%t,"system":%s,"messages":[{"role":"user","content":%q}]}`, stream, tc.system, content))
				req, _, logBody, err := NewDevinExecutor(&tc.cfg).prepareDevinHTTPRequest(t.Context(),
					&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-token"}},
					cliproxyexecutor.Request{Model: "devin/claude-haiku-5-5", Payload: payload},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload, Stream: stream})
				if err != nil {
					t.Fatal(err)
				}
				defer req.Body.Close()
				_, wire, err := helps.ReadConnectFrame(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				_, body, err := helps.FinalizeDevinPayload(wire, func(body []byte) []byte { return body })
				if err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(body, "system_prompt").String(); got != tc.want {
					t.Fatalf("wire system_prompt = %q, want %q", got, tc.want)
				}
				if !bytes.Equal(body, logBody) {
					t.Fatalf("log body differs from wire business payload")
				}
				if got := gjson.GetBytes(body, "prompts.0.content").String(); got != content {
					t.Fatalf("user content = %q, want %q", got, content)
				}
			})
		}
	}
}
