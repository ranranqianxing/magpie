package cursoride

import (
	"math"
	"testing"
)

func catalog() []Model {
	return []Model{
		{ID: "oppo/deepseek-flash", Name: "deepseek flash", Reasoning: true, Efforts: []string{"low", "high"}},
		{ID: "oppo/deepseek-flash-v3", Name: "v3"},
		{ID: "plain/m", Name: "Plain"},
	}
}

func TestRouteUsesTheGatewayID(t *testing.T) {
	r, ok, err := RouteModel(catalog(), "oppo/deepseek-flash", []Parameter{{ID: "reasoning", Value: "high"}})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if r.Model != "oppo/deepseek-flash" || r.DisplayName != "deepseek flash" || r.Effort != "high" {
		t.Fatalf("%+v", r)
	}
	if _, ok, err := RouteModel(catalog(), "claude-4.5", nil); err != nil || ok {
		t.Fatalf("foreign model ok=%v err=%v", ok, err)
	}
	if _, ok, err := RouteModel(catalog(), "", []Parameter{{ID: "fast", Value: "nope"}}); err != nil || ok {
		t.Fatalf("empty id ok=%v err=%v", ok, err)
	}
	r, ok, err = RouteModel(catalog(), "oppo/deepseek-flash", []Parameter{{ID: "effort", Value: "none"}})
	if err != nil || !ok || r.Effort != "none" || r.Model != "oppo/deepseek-flash" {
		t.Fatalf("none %+v ok=%v err=%v", r, ok, err)
	}
}

func TestVariantStringAndLegacySlugResolve(t *testing.T) {
	id := "oppo/deepseek-flash[context=500k,reasoning=low,fast=true]"
	r, ok, err := RouteModel(catalog(), id, []Parameter{{ID: "reasoning", Value: "extra-high"}})
	if err != nil || !ok {
		t.Fatalf("variant ok=%v err=%v", ok, err)
	}
	if r.Model != "oppo/deepseek-flash" || r.Effort != "xhigh" || r.ContextTokens != 500_000 || !r.Fast {
		t.Fatalf("%+v", r)
	}
	r, ok, err = RouteModel(catalog(), "oppo/deepseek-flash-800k-high-fast", nil)
	if err != nil || !ok || r.Model != "oppo/deepseek-flash" || r.Effort != "high" || r.ContextTokens != 800_000 || !r.Fast {
		t.Fatalf("slug ok=%v err=%v route=%+v", ok, err, r)
	}
	r, ok, err = RouteModel(catalog(), "plain/m-200k", nil)
	if err != nil || !ok || r.Model != "plain/m" || r.ContextTokens != 200_000 || r.Effort != "" {
		t.Fatalf("quiet slug %+v ok=%v err=%v", r, ok, err)
	}
	ultra := []Model{{ID: "ds/r", Name: "R", Reasoning: true, Efforts: []string{"ultra"}}}
	r, ok, err = RouteModel(ultra, "ds/r-200k-ultra-fast", nil)
	if err != nil || !ok || r.Model != "ds/r" || r.Effort != "ultra" || !r.Fast {
		t.Fatalf("ultra slug %+v ok=%v err=%v", r, ok, err)
	}
	if _, ok, err := RouteModel(catalog(), "oppo/deepseek-flash-200k-ultra", nil); err != nil || ok {
		t.Fatalf("unlisted effort slug ok=%v err=%v", ok, err)
	}
	// The longer catalog id wins over a shorter id that shares its prefix.
	r, ok, err = RouteModel(catalog(), "oppo/deepseek-flash-v3-200k", nil)
	if err != nil || !ok || r.Model != "oppo/deepseek-flash-v3" {
		t.Fatalf("longer id %+v ok=%v err=%v", r, ok, err)
	}
}

func TestKnownWindowIsTheRouteContext(t *testing.T) {
	models := []Model{{
		ID: "oppo/deepseek-flash", Name: "flash",
		Reasoning: true, Efforts: []string{"low", "high"}, Context: 128000,
	}}
	r, ok, err := RouteModel(models, "oppo/deepseek-flash", nil)
	if err != nil || !ok || r.ContextTokens != 128000 || r.Effort != "" {
		t.Fatalf("bare %+v ok=%v err=%v", r, ok, err)
	}
	r, ok, err = RouteModel(models, "oppo/deepseek-flash[reasoning=high,fast=false]", nil)
	if err != nil || !ok || r.ContextTokens != 128000 || r.Effort != "high" || r.Fast {
		t.Fatalf("variant %+v ok=%v err=%v", r, ok, err)
	}
	r, ok, err = RouteModel(models, "oppo/deepseek-flash-high-fast", nil)
	if err != nil || !ok || r.ContextTokens != 128000 || r.Effort != "high" || !r.Fast {
		t.Fatalf("slug %+v ok=%v err=%v", r, ok, err)
	}
	r, ok, err = RouteModel(models, "oppo/deepseek-flash[context=500k,reasoning=low,fast=true]", nil)
	if err != nil || !ok || r.ContextTokens != 500_000 || r.Effort != "low" || !r.Fast {
		t.Fatalf("override %+v ok=%v err=%v", r, ok, err)
	}
	r, ok, err = RouteModel(catalog(), "plain/m", nil)
	if err != nil || !ok || r.ContextTokens != 0 {
		t.Fatalf("unknown window %+v ok=%v err=%v", r, ok, err)
	}
	atMax := []Model{{ID: "wide/m", Name: "W", Context: math.MaxInt32}}
	r, ok, err = RouteModel(atMax, "wide/m", nil)
	if err != nil || !ok || r.ContextTokens != math.MaxInt32 {
		t.Fatalf("max int32 window %+v ok=%v err=%v", r, ok, err)
	}
}

func TestRunReadyUsesTheCatalogWindow(t *testing.T) {
	if got := agentTokenLimit(t, runReady("oppo/deepseek-flash", "high", 128000)); got != 128000 {
		t.Fatalf("agent_token_limit = %d", got)
	}
	if got := agentTokenLimit(t, runReady("plain/m", "", 0)); got != 0 {
		t.Fatalf("unknown window sent agent_token_limit %d", got)
	}
	if got := agentTokenLimit(t, runReady("plain/m", "", 200000)); got != 200000 {
		t.Fatalf("preset agent_token_limit = %d", got)
	}
	if got := agentTokenLimit(t, runReady("wide/m", "", math.MaxInt32)); got != math.MaxInt32 {
		t.Fatalf("max int32 agent_token_limit = %d", got)
	}
	over := math.MaxInt32
	over++
	if over > 0 && agentTokenLimit(t, runReady("wide/m", "", over)) != 0 {
		t.Fatal("oversized window was sent as agent_token_limit")
	}
}

func agentTokenLimit(t *testing.T, frame []byte) uint64 {
	t.Helper()
	if len(frame) < 5 || frame[0] != 0 {
		t.Fatalf("frame %d", len(frame))
	}
	fs, err := parseFields(frame[5:])
	if err != nil {
		t.Fatal(err)
	}
	ready := fieldBytes(fs, 2)
	if len(ready) != 1 {
		t.Fatalf("run_ready = %d", len(ready))
	}
	rf, err := parseFields(ready[0])
	if err != nil {
		t.Fatal(err)
	}
	meta := fieldBytes(rf, 4)
	if len(meta) != 1 {
		t.Fatalf("metadata = %d", len(meta))
	}
	mf, err := parseFields(meta[0])
	if err != nil {
		t.Fatal(err)
	}
	v, _ := fieldUint(mf, 32)
	return v
}

func TestBadParameterOnOurModelIsAnError(t *testing.T) {
	_, ok, err := RouteModel(catalog(), "plain/m", []Parameter{{ID: "context", Value: "lots"}})
	if err == nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	_, ok, err = RouteModel(catalog(), "plain/m", []Parameter{{ID: "fast", Value: "yes"}})
	if err == nil || ok {
		t.Fatalf("fast ok=%v err=%v", ok, err)
	}
	_, ok, err = RouteModel(catalog(), "claude-4.5", []Parameter{{ID: "context", Value: "lots"}})
	if err != nil || ok {
		t.Fatalf("foreign bad param ok=%v err=%v", ok, err)
	}
	_, _, err = RouteModel(catalog(), "plain/m[context", nil)
	if err == nil {
		t.Fatal("broken variant of our id was treated as unknown")
	}
}
