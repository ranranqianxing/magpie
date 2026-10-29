package cursoride

import (
	"strings"
	"testing"
)

func deepseek() Model {
	return Model{
		ID: "oppo/deepseek-flash", Name: "deepseek flash",
		Reasoning: true, Efforts: []string{"low", "high"}, Images: true, Context: 128000,
	}
}

func TestPickerOmitsBadgesAndKeepsRouteIDs(t *testing.T) {
	m := deepseek()
	raw := AvailableModels([]Model{m})
	if strings.Contains(strings.ToLower(string(raw)), "oppoer") || strings.Contains(string(raw), "Work") {
		t.Fatalf("picker payload leaked a host or a group label: %q", raw)
	}
	if strings.Contains(string(raw), LocalCredential) || strings.Contains(string(raw), "provider-secret") {
		t.Fatalf("picker payload carried a credential: %q", raw)
	}
	fs, err := parseFields(raw)
	if err != nil {
		t.Fatal(err)
	}
	names := fieldStrings(fs, 1)
	if len(names) != 1 || names[0] != m.ID {
		t.Fatalf("model_names = %q", names)
	}
	rows := fieldBytes(fs, 2)
	if len(rows) != 1 {
		t.Fatalf("models = %d", len(rows))
	}
	if containsNum(rows[0], 48) {
		t.Fatal("model_picker_badges is set")
	}
	row, err := parseFields(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fieldString(row, 1); got != m.ID {
		t.Fatalf("name = %q", got)
	}
	if got, _ := fieldString(row, 17); got != m.Name {
		t.Fatalf("client_display_name = %q", got)
	}
	if got, _ := fieldString(row, 18); got != m.ID {
		t.Fatalf("server_model_name = %q", got)
	}
	if got, _ := fieldString(row, 24); got != m.Name || strings.Contains(got, "<span") {
		t.Fatalf("inputbox name = %q", got)
	}
	if v, ok := fieldUint(row, 14); !ok || v != 0 {
		t.Fatalf("supports_max_mode = %d, present %v", v, ok)
	}
	if v, ok := fieldUint(row, 10); !ok || v != 1 {
		t.Fatalf("supports_images = %d, present %v", v, ok)
	}
	ids := paramIDs(t, row)
	if strings.Join(ids, ",") != "context,reasoning,fast" {
		t.Fatalf("parameters = %q", ids)
	}
	efforts := enumValues(t, row, "reasoning")
	if strings.Join(efforts, ",") != "low,high" {
		t.Fatalf("efforts = %q, want the catalog's levels", efforts)
	}
	contexts := enumValues(t, row, "context")
	if len(contexts) != 6 || contexts[5] != "128000" {
		t.Fatalf("contexts = %q", contexts)
	}
	var defs, reprs int
	for _, vraw := range fieldBytes(row, 30) {
		vf, err := parseFields(vraw)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := fieldUint(vf, 4); ok {
			defs++
		}
		repr, _ := fieldString(vf, 9)
		reprs++
		if !strings.HasPrefix(repr, m.ID+"[context=") || strings.Contains(repr, m.Name) {
			t.Fatalf("variant %q does not route by the model id", repr)
		}
		shown, _ := fieldString(vf, 2)
		if strings.Contains(shown, "oppoer") {
			t.Fatalf("variant display leaked a host: %s", shown)
		}
	}
	// 6 contexts, 2 efforts, fast on and off.
	if reprs != 24 || defs != 1 {
		t.Fatalf("variants = %d, defaults = %d", reprs, defs)
	}
}

func TestQuietModelHasNoEffortAxis(t *testing.T) {
	raw := AvailableModels([]Model{{ID: "plain/m", Name: "Plain", Context: 200000}})
	fs, err := parseFields(raw)
	if err != nil {
		t.Fatal(err)
	}
	row, err := parseFields(fieldBytes(fs, 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := fieldUint(row, 9); !ok || v != 0 {
		t.Fatalf("supports_thinking = %d, present %v", v, ok)
	}
	ids := paramIDs(t, row)
	if strings.Join(ids, ",") != "context,fast" {
		t.Fatalf("parameters = %q", ids)
	}
	// 200000 is the 200k preset, so it is not added again.
	if n := len(enumValues(t, row, "context")); n != 5 {
		t.Fatalf("contexts = %d", n)
	}
	var defs int
	for _, vraw := range fieldBytes(row, 30) {
		vf, err := parseFields(vraw)
		if err != nil {
			t.Fatal(err)
		}
		repr, _ := fieldString(vf, 9)
		if strings.Contains(repr, "reasoning=") {
			t.Fatalf("quiet variant names an effort: %s", repr)
		}
		if _, ok := fieldUint(vf, 4); ok {
			defs++
			if !strings.Contains(repr, "context=200k,fast=false") {
				t.Fatalf("default variant = %s", repr)
			}
		}
	}
	if defs != 1 {
		t.Fatalf("defaults = %d", defs)
	}
}

func TestUnknownThinkingLevelsAreLowMediumHigh(t *testing.T) {
	raw := AvailableModels([]Model{{ID: "think/m", Name: "Thinks", Reasoning: true}})
	fs, err := parseFields(raw)
	if err != nil {
		t.Fatal(err)
	}
	row, err := parseFields(fieldBytes(fs, 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(enumValues(t, row, "reasoning"), ","); got != "low,medium,high" {
		t.Fatalf("efforts = %s", got)
	}
}

func TestDisplayNameKeepsUTF8(t *testing.T) {
	const name = "深度求索"
	raw := AvailableModels([]Model{{ID: "ds/v3", Name: name}})
	if !strings.Contains(string(raw), name) {
		t.Fatal("display name was not kept")
	}
	fs, err := parseFields(raw)
	if err != nil {
		t.Fatal(err)
	}
	row, err := parseFields(fieldBytes(fs, 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	got, _ := fieldString(row, 17)
	if got != name {
		t.Fatalf("client_display_name = %q", got)
	}
}

func TestBlankAndDuplicateIDsAreDropped(t *testing.T) {
	raw := AvailableModels([]Model{
		{ID: " ", Name: "nope"},
		{ID: "a/one", Name: "One"},
		{ID: "a/one", Name: "Again"},
		{ID: "a/two"},
	})
	fs, err := parseFields(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := fieldStrings(fs, 1); len(got) != 2 || got[0] != "a/one" || got[1] != "a/two" {
		t.Fatalf("names = %q", got)
	}
	row, err := parseFields(fieldBytes(fs, 2)[1])
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fieldString(row, 17); got != "a/two" {
		t.Fatalf("blank name displayed %q", got)
	}
}

func TestUsableAndDefaultRowsUseTheRouteID(t *testing.T) {
	models := []Model{deepseek(), {ID: "plain/m", Name: "Plain"}}
	usable, err := parseFields(UsableModels(models))
	if err != nil {
		t.Fatal(err)
	}
	rows := fieldBytes(usable, 1)
	if len(rows) != 2 {
		t.Fatalf("usable = %d", len(rows))
	}
	row, err := parseFields(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, num := range []int{1, 3} {
		if got, _ := fieldString(row, num); got != models[0].ID {
			t.Fatalf("field %d = %q", num, got)
		}
	}
	if got, _ := fieldString(row, 4); got != models[0].Name {
		t.Fatalf("display = %q", got)
	}
	creds := fieldBytes(row, 8)
	if len(creds) != 1 {
		t.Fatal("missing credentials")
	}
	cf, err := parseFields(creds[0])
	if err != nil {
		t.Fatal(err)
	}
	key, _ := fieldString(cf, 1)
	if key != LocalCredential || strings.HasPrefix(key, "magpie") {
		t.Fatalf("credential = %q", key)
	}
	if _, ok := fieldString(cf, 2); ok {
		t.Fatal("base_url was set")
	}
	def, err := parseFields(DefaultModel(models))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fieldString(def, 1); got != models[0].ID {
		t.Fatalf("default = %q", got)
	}
	if got, _ := fieldString(def, 2); got != models[0].ID {
		t.Fatalf("thinking default = %q", got)
	}
	if DefaultModel(nil) != nil || DefaultModelForCLI(nil) != nil {
		t.Fatal("empty catalog invented a default")
	}
	nudge, err := parseFields(DefaultModelNudge(models))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fieldString(nudge, 1); got != "0" {
		t.Fatalf("nudge date = %q", got)
	}
	if got := fieldStrings(nudge, 3); len(got) != 2 || got[0] != models[0].ID || got[1] != models[1].ID {
		t.Fatalf("nudge ids = %q", got)
	}
	cli, err := parseFields(DefaultModelForCLI(models))
	if err != nil {
		t.Fatal(err)
	}
	one, err := parseFields(fieldBytes(cli, 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fieldString(one, 1); got != models[0].ID {
		t.Fatalf("cli model = %q", got)
	}
}

func paramIDs(t *testing.T, row []field) []string {
	t.Helper()
	var ids []string
	for _, raw := range fieldBytes(row, 29) {
		pf, err := parseFields(raw)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := fieldString(pf, 1)
		ids = append(ids, id)
	}
	return ids
}

func enumValues(t *testing.T, row []field, id string) []string {
	t.Helper()
	for _, raw := range fieldBytes(row, 29) {
		pf, err := parseFields(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := fieldString(pf, 1)
		if got != id {
			continue
		}
		typ := fieldBytes(pf, 4)
		if len(typ) != 1 {
			t.Fatal("parameter type")
		}
		tf, err := parseFields(typ[0])
		if err != nil {
			t.Fatal(err)
		}
		// enum is field 2, boolean is field 1; values are field 1 inside either.
		box := fieldBytes(tf, 2)
		if id == "fast" {
			box = fieldBytes(tf, 1)
		}
		if len(box) != 1 {
			t.Fatalf("%s type", id)
		}
		bf, err := parseFields(box[0])
		if err != nil {
			t.Fatal(err)
		}
		var vals []string
		for _, one := range fieldBytes(bf, 1) {
			vf, err := parseFields(one)
			if err != nil {
				t.Fatal(err)
			}
			v, _ := fieldString(vf, 1)
			vals = append(vals, v)
		}
		return vals
	}
	t.Fatalf("no parameter %s", id)
	return nil
}

func fieldStrings(fs []field, num int) []string {
	var out []string
	for _, raw := range fieldBytes(fs, num) {
		out = append(out, string(raw))
	}
	return out
}

func containsNum(b []byte, num int) bool {
	fs, err := parseFields(b)
	if err != nil {
		return false
	}
	for _, f := range fs {
		if f.num == num {
			return true
		}
		if f.wt == 2 && containsNum(f.b, num) {
			return true
		}
	}
	return false
}
