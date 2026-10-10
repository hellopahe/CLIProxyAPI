package helps

import (
	"encoding/json"
	"fmt"
	"net/url"
	"runtime"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

const DevinWebSearchPath = "/exa.api_server_pb.ApiServerService/GetWebSearchResults"

// DevinWebSearchRequest is the business payload exposed to payload rules. Domain
// restrictions are enforced on returned URLs; the native RPC has no such fields.
type DevinWebSearchRequest struct {
	Query          string   `json:"query"`
	NumResults     int      `json:"num_results"`
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	BlockedDomains []string `json:"blocked_domains,omitempty"`
}

type DevinSearchHit struct {
	URL     string
	Title   string
	Content string
}

// ParseDevinWebSearch recognizes Claude Code's standalone server-tool request.
// A normal client function named web_search must remain a client function.
func ParseDevinWebSearch(body []byte) (*DevinWebSearchRequest, error) {
	root := gjson.ParseBytes(body)
	tools := root.Get("tools").Array()
	var search gjson.Result
	for _, tool := range tools {
		if strings.HasPrefix(tool.Get("type").String(), "web_search_") {
			search = tool
			break
		}
	}
	if !search.Exists() {
		return nil, nil
	}
	unsupported := func() (*DevinWebSearchRequest, error) {
		return nil, fmt.Errorf("devin web search supports only Claude Code standalone web_search_20250305 requests; mixed tools, dynamic filtering and conversational search are not supported")
	}
	if len(tools) != 1 || search.Get("type").String() != "web_search_20250305" || search.Get("name").String() != "web_search" {
		return unsupported()
	}
	unknownOption := false
	search.ForEach(func(key, _ gjson.Result) bool {
		switch key.String() {
		case "type", "name", "max_uses", "allowed_domains", "blocked_domains", "cache_control":
		default:
			unknownOption = true
		}
		return !unknownOption
	})
	if unknownOption {
		return unsupported()
	}
	if uses := search.Get("max_uses"); uses.Exists() && (uses.Type != gjson.Number || uses.Float() < 1 || uses.Float() != float64(uses.Int())) {
		return nil, fmt.Errorf("devin web search requires max_uses >= 1")
	}
	if choice := root.Get("tool_choice"); choice.Exists() {
		switch choice.Get("type").String() {
		case "auto", "any":
		case "tool":
			if choice.Get("name").String() != "web_search" {
				return unsupported()
			}
		default:
			return unsupported()
		}
	}
	messages := root.Get("messages").Array()
	if len(messages) != 1 || messages[0].Get("role").String() != "user" {
		return unsupported()
	}
	content := messages[0].Get("content")
	text := content.String()
	if content.IsArray() {
		var parts []string
		for _, part := range content.Array() {
			if part.Get("type").String() != "text" {
				return unsupported()
			}
			parts = append(parts, part.Get("text").String())
		}
		text = strings.Join(parts, "\n")
	}
	const prefix = "Perform a web search for the query:"
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(strings.ToLower(text), strings.ToLower(prefix)) {
		return unsupported()
	}
	r := &DevinWebSearchRequest{Query: strings.TrimSpace(text[len(prefix):]), NumResults: 5}
	for field, target := range map[string]*[]string{"allowed_domains": &r.AllowedDomains, "blocked_domains": &r.BlockedDomains} {
		value := search.Get(field)
		if value.Exists() && !value.IsArray() {
			return nil, fmt.Errorf("%s must be an array", field)
		}
		for _, item := range value.Array() {
			if item.Type != gjson.String {
				return nil, fmt.Errorf("%s must contain strings", field)
			}
			*target = append(*target, item.String())
		}
	}
	return r, r.Validate()
}

func (r DevinWebSearchRequest) Validate() error {
	if strings.TrimSpace(r.Query) == "" || len(r.Query) > 16384 {
		return fmt.Errorf("devin web search query must contain 1 to 16384 bytes")
	}
	if r.NumResults < 1 || r.NumResults > 20 {
		return fmt.Errorf("devin web search num_results must be between 1 and 20")
	}
	if len(r.AllowedDomains) > 0 && len(r.BlockedDomains) > 0 {
		return fmt.Errorf("web search allowed_domains and blocked_domains are mutually exclusive")
	}
	for _, domain := range append(append([]string{}, r.AllowedDomains...), r.BlockedDomains...) {
		u, err := url.Parse("https://" + domain)
		if err != nil || u.Hostname() == "" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" || strings.Contains(domain, "://") || strings.ContainsAny(domain, "* \t\r\n") {
			return fmt.Errorf("invalid web search domain restriction %q", domain)
		}
	}
	return nil
}

// Wire fields verified against Devin CLI 3000.11.3: request metadata=1,
// query=2, result count=3. This is unary application/proto, without framing.
func BuildDevinWebSearchRequest(token, seed string, request DevinWebSearchRequest) []byte {
	b := protowire.AppendTag(nil, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, BuildDevinClientMetadataBytes(token, seed, runtime.GOOS))
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendString(b, request.Query)
	b = protowire.AppendTag(b, 3, protowire.VarintType)
	return protowire.AppendVarint(b, uint64(request.NumResults))
}

// Response result=1 contains URL=3, title=4 and page text=7. Unknown fields
// are skipped so new upstream metadata cannot corrupt result decoding.
func ParseDevinWebSearchResponse(body []byte) ([]DevinSearchHit, error) {
	var hits []DevinSearchHit
	err := walkDevinSearchFields(body, func(number protowire.Number, kind protowire.Type, value []byte) error {
		if number != 1 {
			return nil
		}
		if kind != protowire.BytesType {
			return fmt.Errorf("invalid Devin search result wire type")
		}
		var hit DevinSearchHit
		if err := walkDevinSearchFields(value, func(n protowire.Number, k protowire.Type, v []byte) error {
			if n != 3 && n != 4 && n != 7 {
				return nil
			}
			if k != protowire.BytesType {
				return fmt.Errorf("invalid Devin search result field %d", n)
			}
			switch n {
			case 3:
				hit.URL = string(v)
			case 4:
				hit.Title = string(v)
			case 7:
				hit.Content = string(v)
			}
			return nil
		}); err != nil {
			return err
		}
		u, err := url.Parse(hit.URL)
		if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil {
			hits = append(hits, hit)
		}
		return nil
	})
	return hits, err
}

func walkDevinSearchFields(body []byte, visit func(protowire.Number, protowire.Type, []byte) error) error {
	for len(body) > 0 {
		num, kind, n := protowire.ConsumeTag(body)
		if n < 0 {
			return protowire.ParseError(n)
		}
		body = body[n:]
		n = protowire.ConsumeFieldValue(num, kind, body)
		if n < 0 {
			return protowire.ParseError(n)
		}
		var value []byte
		if kind == protowire.BytesType {
			value, _ = protowire.ConsumeBytes(body)
		}
		if err := visit(num, kind, value); err != nil {
			return err
		}
		body = body[n:]
	}
	return nil
}

func (r DevinWebSearchRequest) Filter(hits []DevinSearchHit) []DevinSearchHit {
	out := make([]DevinSearchHit, 0, len(hits))
	matches := func(raw string, domains []string) bool {
		u, _ := url.Parse(raw)
		if u == nil {
			return false
		}
		for _, domain := range domains {
			d, _ := url.Parse("https://" + domain)
			host, target := strings.ToLower(u.Hostname()), strings.ToLower(d.Hostname())
			if (host == target || strings.HasSuffix(host, "."+target)) && strings.HasPrefix(u.EscapedPath(), d.EscapedPath()) {
				return true
			}
		}
		return false
	}
	for _, hit := range hits {
		if len(r.AllowedDomains) > 0 && !matches(hit.URL, r.AllowedDomains) || matches(hit.URL, r.BlockedDomains) {
			continue
		}
		out = append(out, hit)
		if len(out) == r.NumResults {
			break
		}
	}
	return out
}

// BuildDevinClaudeSearchResponse emits real search results, with no fabricated
// model-generated answer, encrypted Anthropic content or LLM token usage.
func BuildDevinClaudeSearchResponse(model, query string, hits []DevinSearchHit, stream bool) []byte {
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	toolID := "srvtoolu_" + id
	results := make([]any, 0, len(hits))
	var text strings.Builder
	for _, hit := range hits {
		results = append(results, map[string]any{"type": "web_search_result", "url": hit.URL, "title": hit.Title, "page_age": nil})
		fmt.Fprintf(&text, "%s\n%s\n%s\n\n", hit.Title, hit.URL, hit.Content)
	}
	if len(hits) == 0 {
		text.WriteString("No web search results were returned.")
	}
	content := []any{
		map[string]any{"type": "server_tool_use", "id": toolID, "name": "web_search", "input": map[string]string{"query": query}},
		map[string]any{"type": "web_search_tool_result", "tool_use_id": toolID, "content": results},
		map[string]any{"type": "text", "text": strings.TrimSpace(text.String())},
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0, "server_tool_use": map[string]int{"web_search_requests": 1}}
	message := map[string]any{"id": "msg_" + id, "type": "message", "role": "assistant", "model": model, "content": content, "stop_reason": "end_turn", "stop_sequence": nil, "usage": usage}
	if !stream {
		b, _ := json.Marshal(message)
		return b
	}
	var out strings.Builder
	emit := func(kind string, data map[string]any) {
		data["type"] = kind
		b, _ := json.Marshal(data)
		fmt.Fprintf(&out, "event: %s\ndata: %s\n\n", kind, b)
	}
	message["content"] = []any{}
	message["stop_reason"] = nil
	message["usage"] = map[string]int{"input_tokens": 0, "output_tokens": 0}
	emit("message_start", map[string]any{"message": message})
	for i, block := range content {
		switch i {
		case 0:
			emit("content_block_start", map[string]any{"index": i, "content_block": map[string]any{"type": "server_tool_use", "id": toolID, "name": "web_search", "input": map[string]any{}}})
			input, _ := json.Marshal(map[string]string{"query": query})
			emit("content_block_delta", map[string]any{"index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
		case 2:
			emit("content_block_start", map[string]any{"index": i, "content_block": map[string]any{"type": "text", "text": ""}})
			emit("content_block_delta", map[string]any{"index": i, "delta": map[string]any{"type": "text_delta", "text": strings.TrimSpace(text.String())}})
		default:
			emit("content_block_start", map[string]any{"index": i, "content_block": block})
		}
		emit("content_block_stop", map[string]any{"index": i})
	}
	emit("message_delta", map[string]any{"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": usage})
	emit("message_stop", map[string]any{})
	return []byte(out.String())
}
