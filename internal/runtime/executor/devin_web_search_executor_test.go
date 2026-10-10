package executor

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

const standaloneDevinSearch = `{"model":"devin/claude-haiku-5-5","messages":[{"role":"user","content":"Perform a web search for the query: example"}],"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":5}]}`

func TestDevinNativeSearchExecution(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != helps.DevinWebSearchPath || r.Header.Get("Content-Type") != "application/proto" || r.Header.Get("Connect-Protocol-Version") != "1" {
					t.Errorf("unexpected search request: %s %v", r.URL.Path, r.Header)
				}
				if r.Header.Get("Authorization") != "Basic test-token-test-token" || r.Header.Get("Sentry-Trace") != "" {
					t.Error("wrong unary authentication/trace headers")
				}
				body, _ := io.ReadAll(r.Body)
				// Decode independently of the production codec. Configured values must
				// reach the wire unchanged after the final payload barrier.
				var query string
				var count uint64
				for len(body) > 0 {
					num, kind, n := protowire.ConsumeTag(body)
					if n < 0 {
						t.Fatal("invalid protobuf tag")
					}
					body = body[n:]
					if num == 2 {
						v, _ := protowire.ConsumeBytes(body)
						query = string(v)
					}
					if num == 3 {
						count, _ = protowire.ConsumeVarint(body)
					}
					n = protowire.ConsumeFieldValue(num, kind, body)
					if n < 0 {
						t.Fatal("invalid protobuf value")
					}
					body = body[n:]
				}
				if query != "configured query" || count != 2 {
					t.Errorf("final payload overwritten: query=%q count=%d", query, count)
				}
				fixture, _ := hex.DecodeString("0a1e1a1368747470733a2f2f6578616d706c652e636f6d2201583a0474657874")
				w.Header().Set("Content-Type", "application/proto")
				_, _ = w.Write(fixture)
			}))
			defer upstream.Close()
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "devin/claude-haiku-5-5", Protocol: "devin"}}, Params: map[string]any{"query": "configured query", "num_results": 2}}}}}
			exec := NewDevinExecutor(cfg)
			auth := &cliproxyauth.Auth{ID: "test", Provider: "devin", Attributes: map[string]string{"api_key": "test-token", "base_url": upstream.URL}}
			req := cliproxyexecutor.Request{Model: "devin/claude-haiku-5-5", Payload: []byte(standaloneDevinSearch)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: req.Payload, Stream: stream}
			var body []byte
			if stream {
				res, err := exec.ExecuteStream(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range res.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					body = append(body, chunk.Payload...)
				}
				if !bytes.Contains(body, []byte(`"type":"web_search_tool_result"`)) || !bytes.Contains(body, []byte("event: message_stop")) {
					t.Fatalf("invalid SSE: %s", body)
				}
			} else {
				res, err := exec.Execute(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				body = res.Payload
				if gjson.GetBytes(body, "content.1.content.0.url").String() != "https://example.com" || gjson.GetBytes(body, "usage.server_tool_use.web_search_requests").Int() != 1 {
					t.Fatalf("invalid JSON: %s", body)
				}
			}
			if calls.Load() != 1 || !bytes.Contains(body, []byte("https://example.com")) {
				t.Fatalf("calls=%d body=%s", calls.Load(), body)
			}
		})
	}
}

func TestDevinNativeSearchErrorsBeforeStream(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantStatus int
		scoped     bool
	}{
		{"policy", 403, `{"code":"permission_denied","message":"Your request was blocked by our content policy."}`, 403, true},
		{"credential", 403, `{"code":"permission_denied","message":"account disabled"}`, 403, false},
		{"quota", 429, `{"message":"quota exhausted"}`, 429, false},
		{"invalid protobuf", 200, "{not protobuf}", 502, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			e := NewDevinExecutor(&config.Config{})
			a := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-token", "base_url": ts.URL}}
			r, err := e.ExecuteStream(context.Background(), a, cliproxyexecutor.Request{Model: "devin/claude-haiku-5-5", Payload: []byte(standaloneDevinSearch)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
			var status interface{ StatusCode() int }
			if r != nil || !errors.As(err, &status) || status.StatusCode() != tc.wantStatus {
				t.Fatalf("result=%v error=%v", r, err)
			}
			var scope interface{ IsRequestScoped() bool }
			if got := errors.As(err, &scope) && scope.IsRequestScoped(); got != tc.scoped {
				t.Fatalf("request scope=%v want=%v", got, tc.scoped)
			}
		})
	}
}

func TestDevinPolicyTrailerPreservesRejectionWithoutCooldown(t *testing.T) {
	for _, message := range []string{"Your request was blocked by our content policy.", "account disabled"} {
		trailer := []byte(`{"error":{"code":"permission_denied","message":"` + message + `"}}`)
		_, _, err := consumeDevinFramesToInteractions(bytes.NewReader(helps.WrapConnectEnvelopeWithFlag(helps.ConnectFlagEndStream, trailer)), "devin/claude-haiku-5-5", "claude-haiku-5-5-medium")
		var status interface{ StatusCode() int }
		if !errors.As(err, &status) || status.StatusCode() != 403 || !strings.Contains(err.Error(), message) {
			t.Fatalf("refusal changed: %v", err)
		}
		var scope interface{ IsRequestScoped() bool }
		got := errors.As(err, &scope) && scope.IsRequestScoped()
		if got != strings.Contains(message, "content policy") {
			t.Fatalf("wrong error scope: %v", err)
		}
	}
}

func TestDevinNativeSearchCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }))
	defer ts.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		e := NewDevinExecutor(&config.Config{})
		a := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-token", "base_url": ts.URL}}
		_, err := e.Execute(ctx, a, cliproxyexecutor.Request{Model: "devin/claude-haiku-5-5", Payload: []byte(standaloneDevinSearch)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
		result <- err
	}()
	<-entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}
