package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func devinStandaloneWebSearch(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*helps.DevinWebSearchRequest, error) {
	if opts.SourceFormat != sdktranslator.FormatClaude {
		return nil, nil
	}
	search, err := helps.ParseDevinWebSearch(req.Payload)
	if err != nil {
		return nil, statusErr{code: http.StatusBadRequest, msg: err.Error()}
	}
	if search != nil && cliproxyexecutor.ResponseFormatOrSource(opts) != sdktranslator.FormatClaude {
		return nil, statusErr{code: http.StatusBadRequest, msg: "Devin standalone web search requires a Claude response format"}
	}
	return search, nil
}

// executeWebSearch handles Claude Code's dedicated search sub-request using
// Devin's own search RPC and the selected Devin credential. No alternate model
// or provider is selected. This path issues no GetChatMessage request; the
// native search RPC does not report LLM token usage.
func (e *DevinExecutor) executeWebSearch(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, search helps.DevinWebSearchRequest, stream bool) (resp cliproxyexecutor.Response, err error) {
	token, baseURL, seed := devinAuthCredentials(auth)
	if token == "" {
		return resp, fmt.Errorf("devin credentials missing: api_key or session_token required")
	}
	model := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, model, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetUpstreamModel("devin/web-search")

	body, errMarshal := json.Marshal(search)
	if errMarshal != nil {
		return resp, errMarshal
	}
	// User payload rules are the last semantic barrier. Serialization below adds
	// only wire fields and authentication metadata, never defaults or sanitizers.
	body = helps.NewPayloadFinalizer(e.cfg, e.Identifier(), model, "devin", "", body, req, opts)(body)
	var configured helps.DevinWebSearchRequest
	if errDecode := json.Unmarshal(body, &configured); errDecode != nil {
		return resp, statusErr{code: http.StatusBadRequest, msg: "invalid configured Devin search payload: " + errDecode.Error()}
	}
	if errValidate := configured.Validate(); errValidate != nil {
		return resp, statusErr{code: http.StatusBadRequest, msg: errValidate.Error()}
	}
	wire := helps.BuildDevinWebSearchRequest(token, seed, configured)
	httpReq, errNew := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+helps.DevinWebSearchPath, bytes.NewReader(wire))
	if errNew != nil {
		return resp, errNew
	}
	if errPrepare := e.PrepareRequest(httpReq, auth); errPrepare != nil {
		return resp, errPrepare
	}
	authID, authLabel, authType, authValue := devinAuthLogFields(auth)
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL: httpReq.URL.String(), Method: http.MethodPost, Headers: httpReq.Header.Clone(),
		Body: body, Provider: e.Identifier(), AuthID: authID, AuthLabel: authLabel, AuthType: authType, AuthValue: authValue,
	})
	client := reporter.TrackHTTPClient(helps.NewDevinHTTPClient(ctx, e.cfg, auth, 0))
	r, errDo := client.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return resp, errDo
	}
	defer func() { _ = r.Body.Close() }()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, r.StatusCode, r.Header.Clone())
	const maxBody = 8 << 20
	responseBody, errRead := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if errRead != nil {
		return resp, errRead
	}
	if len(responseBody) > maxBody {
		return resp, statusErr{code: http.StatusBadGateway, msg: "Devin search response exceeds 8 MiB"}
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		helps.AppendAPIResponseChunk(ctx, e.cfg, responseBody)
		return resp, helps.ScopeDevinRequestError(newDevinStatusError(r.StatusCode, r.Header, responseBody))
	}
	hits, errParse := helps.ParseDevinWebSearchResponse(responseBody)
	if errParse != nil {
		return resp, statusErr{code: http.StatusBadGateway, msg: "invalid Devin search response: " + errParse.Error()}
	}
	hits = configured.Filter(hits)
	logBody, _ := json.Marshal(hits)
	helps.AppendAPIResponseChunk(ctx, e.cfg, logBody)
	reporter.EnsurePublished(ctx)
	headers := make(http.Header)
	headers.Set("X-CPA-Search-Provider", "devin")
	return cliproxyexecutor.Response{
		Payload: helps.BuildDevinClaudeSearchResponse(req.Model, configured.Query, hits, stream),
		Headers: headers,
	}, nil
}
