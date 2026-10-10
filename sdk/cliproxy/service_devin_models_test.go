package cliproxy

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestDevinRegistersOriginalClaudeNamesWithoutAliases(t *testing.T) {
	const id = "devin-original-claude-names"
	r := registry.GetGlobalRegistry()
	t.Cleanup(func() { r.UnregisterClient(id) })
	s := &Service{cfg: &config.Config{}}
	s.registerModelsForAuth(t.Context(), &coreauth.Auth{ID: id, Provider: "devin", Status: coreauth.StatusActive})
	for _, name := range []string{"claude-sonnet-5", "claude-sonnet-5-5", "claude-opus-5-5", "claude-haiku-5-5", "claude-fable-5-1"} {
		if !r.ClientSupportsModel(id, name) {
			t.Errorf("original model %q is not routable without aliases", name)
		}
		if r.ClientSupportsModel(id, "devin/"+name) {
			t.Errorf("duplicate prefixed model %q is exposed", name)
		}
	}
	for _, model := range r.GetModelsForClient(id) {
		if strings.HasPrefix(model.ID, "devin/claude-") {
			t.Errorf("prefixed Claude model leaked into registry: %q", model.ID)
		}
	}
	if !r.ClientSupportsModel(id, "devin/gpt-6-astra") || r.ClientSupportsModel(id, "gpt-6-astra") {
		t.Fatal("Devin GPT model must not take over the Codex model ID")
	}
}
