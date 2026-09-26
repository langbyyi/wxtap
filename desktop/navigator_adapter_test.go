package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/langbyyi/wxtap/desktop/internal/navigator"
)

// navCore answers the navigator's evaluate with a config whose page lists are
// absent, the shape a mini program reports before window.nav has loaded.
type navCore struct{}

func (navCore) Evaluate(context.Context, string, int) (any, error) { return "{}", nil }
func (navCore) EvaluateAwait(context.Context, string, int) (any, error) {
	return "{}", nil
}
func (navCore) InstallHook(context.Context, string) error { return nil }

func TestNavigatorPagesNormalisesMissingLists(t *testing.T) {
	adapter := navigatorAdapter{nav: navigator.New(navCore{}, "wxone")}

	config, err := adapter.FetchConfig(context.Background())
	if err != nil {
		t.Fatalf("FetchConfig: %v", err)
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The navigator always answers with arrays; null would break list consumers.
	if !strings.Contains(string(data), `"pages":[]`) || !strings.Contains(string(data), `"tab_bar_pages":[]`) {
		t.Fatalf("absent page lists must marshal as empty arrays: %s", data)
	}
}
