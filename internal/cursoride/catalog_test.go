package cursoride

import (
	"encoding/json"
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
	if got, _ := fieldString(row, 17); got != m.Name+" · Magpie" {
		t.Fatalf("client_display_name = %q", got)
	}
	if got, _ := fieldString(row, 18); got != m.ID {
		t.Fatalf("server_model_name = %q", got)
	}
	if got, _ := fieldString(row, 24); got != m.Name+" · Magpie" || strings.Contains(got, "<span") {
		t.Fatalf("inputbox name = %q", got)
	}
	if v, ok := fieldUint(row, 14); !ok || v != 0 {
		t.Fatalf("supports_max_mode = %d, present %v", v, ok)
	}
	// The compact picker (Use Multiple Models) keeps a row only when both
	// of these are set. A named section alone is Show all models.
	if v, ok := fieldUint(row, 2); !ok || v != 1 {
		t.Fatalf("default_on = %d, present %v", v, ok)
	}
	if v, ok := fieldUint(row, 40); !ok || v != 1 {
		t.Fatalf("visible_in_routed_model_view = %d, present %v", v, ok)
	}
	if v, ok := fieldUint(row, 10); !ok || v != 1 {
		t.Fatalf("supports_images = %d, present %v", v, ok)
	}
	if v, ok := fieldUint(row, 15); !ok || v != 128000 {
		t.Fatalf("context_token_limit = %d, present %v", v, ok)
	}
	ids := paramIDs(t, row)
	if strings.Join(ids, ",") != "reasoning,fast" {
		t.Fatalf("parameters = %q", ids)
	}
	efforts := enumValues(t, row, "reasoning")
	if strings.Join(efforts, ",") != "low,high" {
		t.Fatalf("efforts = %q, want the catalog's levels", efforts)
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
		if strings.Contains(repr, "context=") || strings.Contains(repr, m.Name) || !strings.HasPrefix(repr, m.ID+"[") {
			t.Fatalf("variant %q does not route by the model id", repr)
		}
		shown, _ := fieldString(vf, 2)
		if strings.Contains(shown, "oppoer") {
			t.Fatalf("variant display leaked a host: %s", shown)
		}
	}
	// The 128k window is not a menu. 2 efforts, fast on and off.
	if reprs != 4 || defs != 1 {
		t.Fatalf("variants = %d, defaults = %d", reprs, defs)
	}
}

func TestQuietModelHasNoEffortAxis(t *testing.T) {
	raw := AvailableModels([]Model{{ID: "plain/m", Name: "Plain"}})
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
	if _, ok := fieldUint(row, 15); ok {
		t.Fatal("unknown window invented context_token_limit")
	}
	ids := paramIDs(t, row)
	if strings.Join(ids, ",") != "context,fast" {
		t.Fatalf("parameters = %q", ids)
	}
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

func TestKnownWindowIsNotAContextMenu(t *testing.T) {
	raw := AvailableModels([]Model{{ID: "plain/m", Name: "Plain", Context: 200000}})
	fs, err := parseFields(raw)
	if err != nil {
		t.Fatal(err)
	}
	row, err := parseFields(fieldBytes(fs, 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := fieldUint(row, 15); !ok || v != 200000 {
		t.Fatalf("context_token_limit = %d, present %v", v, ok)
	}
	ids := paramIDs(t, row)
	if strings.Join(ids, ",") != "fast" {
		t.Fatalf("parameters = %q", ids)
	}
	var defs int
	for _, vraw := range fieldBytes(row, 30) {
		vf, err := parseFields(vraw)
		if err != nil {
			t.Fatal(err)
		}
		repr, _ := fieldString(vf, 9)
		if strings.Contains(repr, "context=") {
			t.Fatalf("known window still offers a context menu: %s", repr)
		}
		if _, ok := fieldUint(vf, 4); ok {
			defs++
			if repr != "plain/m[fast=false]" {
				t.Fatalf("default variant = %s", repr)
			}
		}
	}
	if defs != 1 {
		t.Fatalf("defaults = %d", defs)
	}
	body, err := CompileChat(Route{Model: "plain/m", ContextTokens: 200000}, "c", invoke("inv", textMsg(1, "hi")))
	if err != nil {
		t.Fatal(err)
	}
	rawJSON, err := body.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rawJSON, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["context"]; ok {
		t.Fatalf("chat body gained a context field: %s", rawJSON)
	}
	if _, ok := top["context_tokens"]; ok {
		t.Fatalf("chat body gained context_tokens: %s", rawJSON)
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
	if got != name+" · Magpie" {
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
	if got, _ := fieldString(row, 17); got != "a/two · Magpie" {
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
	if got, _ := fieldString(row, 4); got != models[0].Name+" · Magpie" {
		t.Fatalf("display = %q", got)
	}
	if got, _ := fieldString(row, 5); got != models[0].Name+" · Magpie" {
		t.Fatalf("short display = %q", got)
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

func TestInjectedNamesStayOffOfficialLabelsAndRouteIDs(t *testing.T) {
	grok := Model{ID: "relay/grok-4.7", Name: "Grok 4.7", Reasoning: true, Efforts: []string{"high"}}
	claude := Model{ID: "relay/claude-opus-4.6", Name: "Claude Opus 4.6"}
	composer := Model{ID: "relay/composer-2.5", Name: "Composer 2.5"}
	marked := Model{ID: "relay/already", Name: "Grok 4.7 · Magpie"}
	raw := AvailableModels([]Model{grok, claude, composer, marked})
	fs, err := parseFields(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := fieldStrings(fs, 1); len(got) != 4 || got[0] != grok.ID || got[1] != claude.ID || got[2] != composer.ID || got[3] != marked.ID {
		t.Fatalf("model_names = %q", got)
	}
	rows := fieldBytes(fs, 2)
	if len(rows) != 4 {
		t.Fatalf("models = %d", len(rows))
	}
	for i, m := range []Model{grok, claude, composer, marked} {
		row, err := parseFields(rows[i])
		if err != nil {
			t.Fatal(err)
		}
		id, _ := fieldString(row, 1)
		server, _ := fieldString(row, 18)
		shown, _ := fieldString(row, 17)
		short, _ := fieldString(row, 24)
		tag, _ := fieldString(row, 39)
		if id != m.ID || server != m.ID {
			t.Fatalf("route id %q server %q", id, server)
		}
		if strings.Contains(id, pickerMark) || strings.Contains(server, pickerMark) || strings.Contains(id, "Magpie") {
			t.Fatalf("route id picked up the picker label: %q", id)
		}
		want := m.Name + " · Magpie"
		if strings.HasSuffix(m.Name, " · Magpie") {
			want = m.Name
		}
		if shown != want || short != want {
			t.Fatalf("display %q short %q, want %q", shown, short, want)
		}
		if shown == "Grok 4.7" || shown == "Claude Opus 4.6" || shown == "Composer 2.5" {
			t.Fatalf("display stayed an official name %q", shown)
		}
		if tag != "Magpie" {
			t.Fatalf("tagline = %q", tag)
		}
		if v, ok := fieldUint(row, 2); !ok || v != 1 {
			t.Fatalf("default_on = %d present %v", v, ok)
		}
		if v, ok := fieldUint(row, 38); !ok || v != 1 {
			t.Fatalf("named section = %d present %v", v, ok)
		}
		if v, ok := fieldUint(row, 40); !ok || v != 1 {
			t.Fatalf("visible_in_routed_model_view = %d present %v", v, ok)
		}
		if containsNum(rows[i], 48) {
			t.Fatal("model_picker_badges is set")
		}
		vendor, _ := fieldString(row, 41)
		if vendor != pickerSubtitle {
			t.Fatalf("vendor_name = %q", vendor)
		}
		vb := fieldBytes(row, 42)
		if len(vb) != 1 {
			t.Fatal("vendor")
		}
		vf, err := parseFields(vb[0])
		if err != nil {
			t.Fatal(err)
		}
		if v, ok := fieldUint(vf, 1); !ok || v != modelVendorCursor {
			t.Fatalf("vendor id = %d present %v", v, ok)
		}
		if got, _ := fieldString(vf, 2); got != pickerSubtitle {
			t.Fatalf("vendor display = %q", got)
		}
		tip := fieldBytes(row, 8)
		if len(tip) != 1 {
			t.Fatal("tooltip")
		}
		tf, err := parseFields(tip[0])
		if err != nil {
			t.Fatal(err)
		}
		if primary, _ := fieldString(tf, 1); primary != want {
			t.Fatalf("tooltip title = %q", primary)
		}
		if secondary, _ := fieldString(tf, 2); secondary != pickerSubtitle {
			t.Fatalf("tooltip subtitle = %q", secondary)
		}
		md, _ := fieldString(tf, 7)
		if md == m.Name || !strings.Contains(md, want) || !strings.Contains(md, pickerSubtitle) {
			t.Fatalf("tooltip markdown = %q", md)
		}
		var sawFast bool
		for _, vraw := range fieldBytes(row, 30) {
			vf, err := parseFields(vraw)
			if err != nil {
				t.Fatal(err)
			}
			repr, _ := fieldString(vf, 9)
			if !strings.HasPrefix(repr, m.ID+"[context=") || strings.Contains(repr, pickerMark) || strings.Contains(repr, "Magpie") {
				t.Fatalf("variant route %q", repr)
			}
			label, _ := fieldString(vf, 2)
			outside, _ := fieldString(vf, 8)
			line, _ := fieldString(vf, 7)
			if !strings.Contains(label, want) || !strings.Contains(outside, want) || line != pickerSubtitle {
				t.Fatalf("variant label %q outside %q tagline %q", label, outside, line)
			}
			if strings.Contains(repr, "fast=") {
				sawFast = true
			}
		}
		if !sawFast {
			t.Fatal("fast axis missing")
		}
		routed, ok, err := RouteModel([]Model{m}, m.ID, nil)
		if err != nil || !ok || routed.Model != m.ID || routed.DisplayName != m.Name {
			t.Fatalf("route %+v ok=%v err=%v", routed, ok, err)
		}
	}
	usable, err := parseFields(UsableModels([]Model{grok}))
	if err != nil {
		t.Fatal(err)
	}
	urows := fieldBytes(usable, 1)
	if len(urows) != 1 {
		t.Fatalf("usable = %d", len(urows))
	}
	urow, err := parseFields(urows[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, num := range []int{1, 3} {
		if got, _ := fieldString(urow, num); got != grok.ID {
			t.Fatalf("usable field %d = %q", num, got)
		}
	}
	for _, num := range []int{4, 5} {
		if got, _ := fieldString(urow, num); got != "Grok 4.7 · Magpie" {
			t.Fatalf("usable display field %d = %q", num, got)
		}
	}

	official := officialRow("grok-4.7", "Grok 4.7", func(b *wbuf) {
		b.str(39, "Cursor's Grok")
	})
	merged, err := MergeUnary(catalogMessage(official), AvailableModels([]Model{grok}))
	if err != nil {
		t.Fatal(err)
	}
	off := mustParse(t, string(rowByName(t, string(merged), "grok-4.7")))
	got, _ := fieldString(off, 17)
	if got != "Grok 4.7" || strings.Contains(got, pickerMark) {
		t.Fatalf("official display rewritten: %q", got)
	}
	if tag, _ := fieldString(off, 39); tag != "Cursor's Grok" {
		t.Fatalf("official tagline rewritten: %q", tag)
	}
	if routedOn(t, rowByName(t, string(merged), "grok-4.7")) {
		t.Fatal("merge stamped the compact flag onto an official row")
	}
	inj := mustParse(t, string(rowByName(t, string(merged), grok.ID)))
	if shown, _ := fieldString(inj, 17); shown != "Grok 4.7 · Magpie" {
		t.Fatalf("injected display = %q", shown)
	}
	if !routedOn(t, rowByName(t, string(merged), grok.ID)) {
		t.Fatal("injected row lost the compact flag")
	}
	promoted := promoteCloudCompact(merged)
	off = mustParse(t, string(rowByName(t, string(promoted), "grok-4.7")))
	if got, _ := fieldString(off, 17); got != "Grok 4.7" {
		t.Fatalf("cloud promotion changed the official name: %q", got)
	}
	if tag, _ := fieldString(off, 39); tag != "Cursor's Grok" {
		t.Fatalf("cloud promotion changed the official tagline: %q", tag)
	}
	if !routedOn(t, rowByName(t, string(promoted), "grok-4.7")) {
		t.Fatal("cloud compact left the official row out")
	}
	inj = mustParse(t, string(rowByName(t, string(promoted), grok.ID)))
	if shown, _ := fieldString(inj, 17); shown != "Grok 4.7 · Magpie" {
		t.Fatalf("cloud promotion changed the injected name: %q", shown)
	}
	if id, _ := fieldString(inj, 1); id != grok.ID {
		t.Fatalf("cloud promotion changed the route id: %q", id)
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
