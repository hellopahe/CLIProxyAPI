# Devin search for Claude Code

Claude Code sends its built-in WebSearch operation as a separate Anthropic
Messages request containing only a `web_search_20250305` server tool and a user
message beginning `Perform a web search for the query:`. Previously the Devin
adapter converted that tool into an ordinary function. No component executed
the function, so Claude Code could report "Did 0 searches" with no sources.

The Devin executor now recognizes this standalone request before protocol
translation and calls Devin's own `GetWebSearchResults` RPC with the selected
Devin credential and the existing proxy configuration. It returns actual search
results as Claude `server_tool_use` and `web_search_tool_result` blocks, followed
by the result titles, URLs and page excerpts. Both JSON and SSE responses work.
There is no cross-provider fallback, alternate model, extra API key, or plugin
configuration. Ordinary chat and client function tools keep the existing path.

This is a CPA compatibility implementation against a Devin client backend,
not an Anthropic-hosted search or an official Cognition integration guarantee.
Results have no Anthropic `encrypted_content`; CPA does not invent one. These
standalone tool responses should not be replayed to Anthropic's own Messages API.

## Supported request shape

```json
{
  "model": "devin/claude-haiku-5-5",
  "messages": [{
    "role": "user",
    "content": "Perform a web search for the query: Go release notes"
  }],
  "tools": [{
    "type": "web_search_20250305",
    "name": "web_search",
    "max_uses": 8
  }],
  "tool_choice": {"type": "auto"},
  "max_tokens": 2048,
  "stream": true
}
```

- String and text-block user content are supported. Exactly one search is run;
  `max_uses` must permit at least one use and is not interpreted as a result count.
- Five results are requested by default. No inference call generates an answer.
  The response records one search request and zero reported inference tokens;
  this is not a claim that the upstream search service is free.
- `allowed_domains` and `blocked_domains` are mutually exclusive result filters.
  A domain matches itself and its subdomains, with an optional path prefix.
  Filtering is applied to the returned result URLs, so it can leave fewer than
  five results or an empty set. No search-engine-side domain restriction is claimed.
- Normal function tools named `web_search` remain client-executed tools.
- Mixed tool lists, conversation history, arbitrary search prompts, dynamic
  filtering versions, localization and unsupported tool options return 400
  instead of silently degrading to an unexecuted function.
- Payload rules for protocol `devin` can configure the final `query`,
  `num_results` (1–20), `allowed_domains`, and `blocked_domains`. They run once,
  after built-in preparation and before wire encoding. Invalid configured
  values fail before an upstream call.
- Cancellation, the configured proxy and upstream HTTP errors are preserved.
  Streaming does not begin until the unary search succeeds.

## Request-specific content refusals

The exact Devin 403 message `Your request was blocked by our content policy`
is treated as request-scoped. Its status and message still reach the client;
CPA neither rewrites the prompt nor retries around the refusal. It no longer
cools unrelated requests to the same credential/model for 30 minutes. Other
authentication, account, quota and capacity errors retain their existing handling.
This also prevents a failed session-title request from poisoning subsequent
WebSearch and WebFetch helper requests. It does not fix an upstream refusal of
the WebFetch helper request itself.

## Protocol evidence and validation

The unary protocol was observed from Devin CLI **3000.11.3** on 2026-10-10 with
an operator-authorized account and a public GitHub query:

- `POST /exa.api_server_pb.ApiServerService/GetWebSearchResults`
- `Content-Type: application/proto`, `Connect-Protocol-Version: 1`
- Request protobuf: client metadata field 1, query field 2, result count field 3.
- Response protobuf: repeated result field 1; each result has URL field 3,
  title field 4, and page text field 7. Other metadata fields are ignored.

Offline tests cover fixed wire fixtures, result framing, domain filtering,
client-tool preservation, unsupported requests, payload overrides, cancellation,
malformed upstream responses, and preservation of policy/auth/quota errors.
No account credentials, captured metadata or private request payloads are included.

```sh
go test ./internal/runtime/executor/... ./sdk/cliproxy/auth -count=1
go test -race ./internal/runtime/executor/... -run 'TestDevin(NativeSearch|PolicyTrailer|WebSearch)' -count=1
go build -o test-output ./cmd/server
```
