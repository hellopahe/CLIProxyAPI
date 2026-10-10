package helps

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"google.golang.org/protobuf/encoding/protowire"
)

const devinSearchTestRequest = `{"model":"devin/claude-haiku-5-5","messages":[{"role":"user","content":"Perform a web search for the query: golang release"}],"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":5}]}`

func TestDevinWebSearchRecognition(t *testing.T) {
	r, err := ParseDevinWebSearch([]byte(devinSearchTestRequest))
	if err != nil || r == nil || r.Query != "golang release" || r.NumResults != 5 {
		t.Fatalf("request = %+v, error = %v", r, err)
	}
	for _, tc := range []struct {
		path      string
		value     any
		wantError bool
	}{
		{"tools.0.type", "function", false},
		{"tools.0.type", "web_search_20260209", true},
		{"tools.1", map[string]any{"name": "shell", "input_schema": map[string]any{}}, true},
		{"tools.0.max_uses", 0, true},
		{"tools.0.user_location", map[string]any{"type": "approximate"}, true},
		{"messages.0.content", "Search whenever needed during this conversation.", true},
		{"tool_choice", map[string]any{"type": "none"}, true},
		{"tools.0.allowed_domains", []string{"https://example.com"}, true},
	} {
		body, _ := sjson.Set(devinSearchTestRequest, tc.path, tc.value)
		got, err := ParseDevinWebSearch([]byte(body))
		if (err != nil) != tc.wantError || !tc.wantError && got != nil {
			t.Errorf("%s=%v: got %+v, %v", tc.path, tc.value, got, err)
		}
	}
	array, _ := sjson.Set(devinSearchTestRequest, "messages.0.content", []any{map[string]any{"type": "text", "text": "Perform a web search for the query: golang release"}})
	if r, err := ParseDevinWebSearch([]byte(array)); err != nil || r.Query != "golang release" {
		t.Fatalf("text-block query: %+v, %v", r, err)
	}
}

func TestDevinWebSearchWire(t *testing.T) {
	b := BuildDevinWebSearchRequest("test-token", "test-device", DevinWebSearchRequest{Query: "abc", NumResults: 5})
	_, _, tagLen := protowire.ConsumeTag(b)
	_, metadataLen := protowire.ConsumeBytes(b[tagLen:])
	// Fixed bytes independently pin native query field 2 and count field 3.
	want, _ := hex.DecodeString("12036162631805")
	if !bytes.Equal(b[tagLen+metadataLen:], want) {
		t.Fatalf("native search business fields = %x", b[tagLen+metadataLen:])
	}
	// Native response shape: result(1) { url(3), title(4), content(7) }.
	fixture, _ := hex.DecodeString("0a1e1a1368747470733a2f2f6578616d706c652e636f6d2201583a0474657874")
	hits, err := ParseDevinWebSearchResponse(fixture)
	if err != nil || len(hits) != 1 || hits[0].URL != "https://example.com" || hits[0].Title != "X" || hits[0].Content != "text" {
		t.Fatalf("hits = %+v, error = %v", hits, err)
	}
	if _, err := ParseDevinWebSearchResponse(fixture[:len(fixture)-1]); err == nil {
		t.Fatal("truncated upstream protobuf was accepted")
	}
	if hits, err := ParseDevinWebSearchResponse(nil); err != nil || len(hits) != 0 {
		t.Fatalf("empty result: %+v, %v", hits, err)
	}
}

func TestDevinWebSearchDomainFiltering(t *testing.T) {
	hits := []DevinSearchHit{{URL: "https://example.com/docs/a"}, {URL: "https://sub.example.com/docs/b"}, {URL: "https://example.com.evil.test/docs/a"}, {URL: "https://example.com/other"}}
	r := DevinWebSearchRequest{Query: "test", NumResults: 5, AllowedDomains: []string{"example.com/docs/"}}
	if err := r.Validate(); err != nil || len(r.Filter(hits)) != 2 {
		t.Fatalf("allow filter failed: %v, %+v", err, r.Filter(hits))
	}
	r.AllowedDomains = nil
	r.BlockedDomains = []string{"example.com"}
	if got := r.Filter(hits); len(got) != 1 || got[0].URL != "https://example.com.evil.test/docs/a" {
		t.Fatalf("block filter: %+v", got)
	}
}

func TestDevinWebSearchClaudeResponse(t *testing.T) {
	hits := []DevinSearchHit{{URL: "https://example.com", Title: "Example", Content: "Observed page text"}}
	message := BuildDevinClaudeSearchResponse("devin/claude-haiku-5-5", "example", hits, false)
	if gjson.GetBytes(message, "content.0.type").String() != "server_tool_use" || gjson.GetBytes(message, "content.1.content.0.url").String() != hits[0].URL || gjson.GetBytes(message, "content.0.id").String() != gjson.GetBytes(message, "content.1.tool_use_id").String() {
		t.Fatalf("invalid search response: %s", message)
	}
	if gjson.GetBytes(message, "usage.server_tool_use.web_search_requests").Int() != 1 || gjson.GetBytes(message, "usage.output_tokens").Int() != 0 {
		t.Fatalf("invalid usage: %s", message)
	}
	stream := string(BuildDevinClaudeSearchResponse("devin/claude-haiku-5-5", "example", hits, true))
	if !strings.Contains(stream, `"type":"web_search_tool_result"`) || !strings.Contains(stream, `"type":"text_delta"`) || !strings.Contains(stream, "event: message_stop") || strings.Contains(stream, `"type":"tool_use"`) {
		t.Fatalf("invalid search SSE: %s", stream)
	}
}
