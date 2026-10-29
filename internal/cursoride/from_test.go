package cursoride

import (
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestFromEntriesKeepsTheModelAndDropsTheHost(t *testing.T) {
	e := provider.Entry{
		ID: "oppo/deepseek-flash", Model: "deepseek-flash", Name: "deepseek flash",
		Reasoning: true, Efforts: []string{"low", "high"}, Images: true, Context: 128000,
		Provider: provider.Provider{
			ID: "oppo", Name: "Work", Key: "provider-secret",
			Chat: "https://OPPO-CLOUD-LLM-PERSONAL-CN.oppoer.me/v1",
		},
	}
	blank := provider.Entry{ID: " ", Name: "nope"}
	unnamed := provider.Entry{ID: "plain/m", Model: "m"}
	got := FromEntries([]provider.Entry{e, blank, unnamed})
	if len(got) != 2 || got[0].ID != e.ID || got[0].Name != "deepseek flash" || !got[0].Reasoning || !got[0].Images || got[0].Context != 128000 {
		t.Fatalf("%+v", got)
	}
	if len(got[0].Efforts) != 2 || got[0].Efforts[0] != "low" {
		t.Fatalf("efforts %+v", got[0].Efforts)
	}
	if got[1].Name != "m" {
		t.Fatalf("unnamed displayed %q", got[1].Name)
	}
	raw := AvailableModels(got)
	for _, leak := range []string{"oppoer", "Work", "provider-secret", "OPPO-CLOUD"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("catalog contains %q", leak)
		}
	}
	if !strings.Contains(string(raw), "deepseek flash") || !strings.Contains(string(raw), e.ID) {
		t.Fatal("catalog lost the model")
	}
}

func TestRenamedModelShowsTheUsersName(t *testing.T) {
	e := provider.Entry{ID: "ds/v3", Model: "v3", Name: "我的模型", Default: "DeepSeek"}
	got := FromEntries([]provider.Entry{e})
	if len(got) != 1 || got[0].Name != "我的模型" {
		t.Fatalf("%+v", got)
	}
}
