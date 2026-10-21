package engine

import (
	"testing"

	"shingoedge/domain"
)

// resolveLoader resolves a *domain.Loader for a payload through the engine's
// LoaderStore (the Core-owned aggregate), the same way the hot path does.
func resolveLoader(t *testing.T, eng *Engine, payload string) *domain.Loader {
	t.Helper()
	l, err := eng.loaderStore.LoaderForPayload(domain.PayloadCode(payload), domain.RoleProduce)
	if err != nil || l == nil {
		t.Fatalf("resolve loader for %s: loader=%v err=%v", payload, l, err)
	}
	return l
}
