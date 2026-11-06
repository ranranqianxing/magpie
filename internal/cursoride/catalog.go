package cursoride

import (
	"math"
	"strings"
)

// LocalCredential is the api key placed on a usable-model row.
//
// Cursor's CLI reads that row as the model's own credentials. The value
// is a marker so the row is not the user's Cursor subscription and not a
// provider secret. It is not a gateway token either: those are "magpie"
// and "magpie-<agent>". The proxy decides magpie's models by the route
// id, and base_url is left unset so the row names no vendor host.
const LocalCredential = "cursor-magpie-local"

// Model is one magpie catalog entry as Cursor's picker should show it.
//
// ID is the gateway model id, "provider/model" or a routing group's id.
// Cursor gets it as AvailableModel.name and server_model_name, and sends
// that same id back on the inference request. Name is the catalog name.
// The picker shows it with a Magpie suffix (pickerDisplay). The suffix is
// not written on the route id. Name is never a provider host and never a
// routing-group label stuffed into a badge.
type Model struct {
	ID   string
	Name string
	// Reasoning is set when the model thinks, whether or not it has levels.
	Reasoning bool
	// Efforts are the levels the catalog lists. Empty with Reasoning set
	// means the levels are not known; the picker then offers low, medium
	// and high, which is what magpie tells an agent in that case.
	Efforts []string
	Images  bool
	// Context is the prompt window in tokens, when known. A size that
	// fits context_token_limit is that field, and the route's context
	// when the request names none. It is not a menu of other sizes.
	// Zero, or a size that does not fit the int32, is an unknown
	// window: the picker then offers the preset sizes, and 200k is the
	// default, which is the limit Cursor uses when agent_token_limit
	// is absent.
	Context int
}

type effortChoice struct{ value, label string }

type contextChoice struct{ value, label string }

const defaultContext = "200k"

// contextPresets are the context sizes offered when the catalog does
// not name a window. The words are the parameter values Cursor sends
// back. 200k is the default, matching the limit Cursor uses when
// agent_token_limit is absent. A known window is not one of these.
var contextPresets = []contextChoice{
	{"200k", "200K"},
	{"356k", "356K"},
	{"500k", "500K"},
	{"800k", "800K"},
	{"1m", "1M"},
}

var fastChoices = [2]bool{false, true}

// AvailableModels is the protobuf appended to Cursor's AvailableModels
// response: field 1 the route ids, field 2 the rows. Tags are Cursor's
// AvailableModelsResponse.AvailableModel. model_picker_badges (field 48)
// is left out; Cursor draws that field as the blue capsule beside the name.
func AvailableModels(models []Model) []byte {
	return availableModels(models, true)
}

// availableModels encodes the injected catalog. routed sets
// visible_in_routed_model_view. The IDE and Cloud Agents compact
// lists keep a row only when that flag and default_on are both set.
// Automations is a different AvailableModels call, and its client
// drops a row that has the flag. That call leaves the flag unset.
func availableModels(models []Model, routed bool) []byte {
	models = publishable(models)
	var b wbuf
	for _, m := range models {
		b.str(1, m.ID)
	}
	for _, m := range models {
		b.msg(2, encodeModel(m, routed))
	}
	return []byte(b)
}

// UsableModels is the protobuf appended to GetUsableModels. Each row is
// agent.v1.ModelDetails: the route id, the picker display name, and the
// local credential marker. Both of Cursor's usable-model paths take this
// shape. The display name is the catalog name plus the Magpie suffix.
// The route id is not.
func UsableModels(models []Model) []byte {
	var b wbuf
	for _, m := range publishable(models) {
		b.msg(1, encodeUsable(m))
	}
	return []byte(b)
}

// DefaultModel is GetDefaultModelResponse. The model and the thinking
// model are the first route id. An empty catalog encodes nothing, which
// is an empty response, not a made-up model.
func DefaultModel(models []Model) []byte {
	models = publishable(models)
	if len(models) == 0 {
		return nil
	}
	var b wbuf
	b.str(1, models[0].ID)
	b.str(2, models[0].ID)
	return []byte(b)
}

// DefaultModelForCLI is GetDefaultModelForCliResponse, one ModelDetails.
func DefaultModelForCLI(models []Model) []byte {
	models = publishable(models)
	if len(models) == 0 {
		return nil
	}
	var b wbuf
	b.msg(1, encodeUsable(models[0]))
	return []byte(b)
}

// DefaultModelNudge is GetDefaultModelNudgeDataResponse. nudge_date is
// "0" and every route id is listed as not switched. Cursor is not asked
// to move the user onto a magpie model by itself.
func DefaultModelNudge(models []Model) []byte {
	var b wbuf
	b.str(1, "0")
	for _, m := range publishable(models) {
		b.str(3, m.ID)
	}
	return []byte(b)
}

func publishable(models []Model) []Model {
	seen := make(map[string]bool, len(models))
	out := make([]Model, 0, len(models))
	for _, m := range models {
		m.ID = strings.TrimSpace(m.ID)
		m.Name = strings.TrimSpace(m.Name)
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		if m.Name == "" {
			m.Name = m.ID
		}
		if m.Context < 0 {
			m.Context = 0
		}
		out = append(out, m)
	}
	return out
}

func encodeModel(m Model, routed bool) []byte {
	disp := pickerDisplay(m.Name)
	tip := encodeTooltip(disp)
	contexts := contextChoices(m.Context)
	efforts := effortChoices(m)
	defs := encodeParameters(contexts, efforts)
	variants := encodeVariants(m.ID, disp, tip, contexts, efforts)
	var b wbuf
	b.str(1, m.ID)
	b.bool(2, true)
	b.bool(5, true)
	b.u32(6, 0)
	b.msg(8, tip)
	b.bool(9, m.Reasoning)
	b.bool(10, m.Images)
	b.bool(14, false)
	// context_token_limit. The agent host copies this onto the model
	// when it is set. A known catalog window is that limit. An unknown
	// window is left unset, rather than a size this model does not have.
	if n, ok := contextLimit(m.Context); ok {
		b.u32(15, n)
	}
	b.str(17, disp)
	b.str(18, m.ID)
	b.bool(19, true)
	b.msg(20, tip)
	b.bool(21, false)
	b.bool(22, true)
	b.str(24, disp)
	b.bool(25, true)
	b.bool(26, false)
	for _, d := range defs {
		b.msg(29, d)
	}
	for _, v := range variants {
		b.msg(30, v.body)
		b.str(36, v.slug)
	}
	// named_model_section_index. Every injected row shares this index, so
	// Show all models keeps them in one section. The number is an index
	// into Cursor's own section table, not a title this package can set.
	// A different index would point at a missing heading. The title the
	// picker can read is the display-name suffix, the tagline, and the
	// vendor string.
	b.u32(38, 1)
	// tagline, the gray line under the name. Official rows carry their
	// own sentence. Injected rows all say Magpie.
	b.str(39, pickerSubtitle)
	// visible_in_routed_model_view. Cursor's compact picker, including
	// Use Multiple Models, lists a row only when this is set and
	// default_on is set. A named-model section alone leaves the row in
	// Show all models. Official rows keep the flag Cursor sent.
	// Automations reads the same field the other way: excludeRoutedModels
	// drops a row that has it. That request leaves the field unset.
	if routed {
		b.bool(40, true)
	}
	b.str(41, pickerSubtitle)
	b.msg(42, encodeVendor())
	return []byte(b)
}

// pickerSubtitle is the word every injected row shows as its vendor
// line, its tagline, and the end of its display name. One word, not
// each provider's name: a custom provider is often named by its host,
// and that host is what Cursor's badges were painting in blue.
const pickerSubtitle = "Magpie"

// pickerMark is appended to client_display_name, the input-box name,
// and variant display names. Official rows are "Grok 4.7", "Claude …",
// and "Composer …". The compact list shows those beside injected rows
// and does not show the route id. The mark is not written on name or
// server_model_name, which Cursor sends back as the route id.
const pickerMark = " · " + pickerSubtitle

func pickerDisplay(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return pickerSubtitle
	}
	if strings.HasSuffix(name, pickerMark) {
		return name
	}
	return name + pickerMark
}

func encodeVendor() []byte {
	var b wbuf
	b.u32(1, 6) // Cursor's MODEL_VENDOR_ID_CURSOR, the slot these rows use
	b.str(2, pickerSubtitle)
	return []byte(b)
}

func encodeTooltip(display string) []byte {
	var b wbuf
	b.str(1, display)
	b.str(2, pickerSubtitle)
	// Cursor's tooltip markdown is a title, then the one-line blurb.
	b.str(7, display+"<br>"+pickerSubtitle)
	return []byte(b)
}

func encodeUsable(m Model) []byte {
	var cred wbuf
	cred.str(1, LocalCredential)
	var b wbuf
	shown := pickerDisplay(m.Name)
	b.str(1, m.ID)
	b.msg(2, nil) // thinking_details present, with nothing else in it
	b.str(3, m.ID)
	b.str(4, shown)
	b.str(5, shown)
	b.msg(8, cred)
	return []byte(b)
}

// contextChoices is the context parameter, when the window is unknown.
// A window contextLimit accepts is context_token_limit and is not also
// a menu: Cursor's own local-provider rows offer a context parameter
// only for a real long-context split, which this catalog does not have.
// Zero and a size that does not fit the int32 are unknown, so they
// still offer the presets, with 200k as the default.
func contextChoices(tokens int) []contextChoice {
	if _, ok := contextLimit(tokens); ok {
		return nil
	}
	return append([]contextChoice(nil), contextPresets...)
}

// contextLimit is a window that fits agent_token_limit, an int32, and
// context_token_limit. A value that does not fit is unknown: it is not
// truncated into a different window.
func contextLimit(n int) (uint32, bool) {
	if n <= 0 || n > math.MaxInt32 {
		return 0, false
	}
	return uint32(n), true
}

func effortChoices(m Model) []effortChoice {
	if !m.Reasoning {
		return nil
	}
	seen := map[string]bool{}
	var out []effortChoice
	for _, raw := range m.Efforts {
		v := strings.ToLower(strings.TrimSpace(raw))
		if v == "extra-high" {
			v = "xhigh"
		}
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, effortChoice{value: v, label: effortLabel(v)})
	}
	if len(out) == 0 {
		return []effortChoice{{"low", "Low"}, {"medium", "Medium"}, {"high", "High"}}
	}
	return out
}

func effortLabel(v string) string {
	switch v {
	case "minimal":
		return "Minimal"
	case "low":
		return "Low"
	case "medium":
		return "Medium"
	case "high":
		return "High"
	case "xhigh":
		return "Extra High"
	case "max":
		return "Max"
	default:
		return v
	}
}

func defaultEffort(efforts []effortChoice) string {
	for _, want := range []string{"high", "medium"} {
		for _, e := range efforts {
			if e.value == want {
				return e.value
			}
		}
	}
	if len(efforts) == 0 {
		return ""
	}
	return efforts[0].value
}

func encodeParameters(contexts []contextChoice, efforts []effortChoice) [][]byte {
	var out [][]byte
	if len(contexts) > 0 {
		out = append(out, encodeEnumParam("context", "Context",
			"Context size used to trigger conversation compaction.", contexts, false))
	}
	if len(efforts) > 0 {
		vals := make([]contextChoice, len(efforts))
		for i, e := range efforts {
			vals[i] = contextChoice{value: e.value, label: e.label}
		}
		out = append(out, encodeEnumParam("reasoning", "Effort",
			"Effort the model uses to generate its response.", vals, true))
	}
	out = append(out, encodeFastParam())
	return out
}

func encodeEnumParam(id, name, tip string, values []contextChoice, cycle bool) []byte {
	var enum wbuf
	for _, v := range values {
		var one wbuf
		one.str(1, v.value)
		if v.label != "" {
			one.str(2, v.label)
		}
		enum.msg(1, one)
	}
	var typ wbuf
	typ.msg(2, enum)
	return encodeParam(id, name, tip, typ, cycle)
}

func encodeFastParam() []byte {
	var vals wbuf
	var off wbuf
	off.str(1, "false")
	vals.msg(1, off)
	var on wbuf
	on.str(1, "true")
	on.str(2, "Fast")
	on.bool(3, true)
	vals.msg(1, on)
	var typ wbuf
	typ.msg(1, vals)
	return encodeParam("fast", "Fast", "Significantly faster but consumes more usage.", typ, false)
}

func encodeParam(id, name, tip string, typ wbuf, cycle bool) []byte {
	var b wbuf
	b.str(1, id)
	b.str(2, name)
	b.str(3, tip)
	b.msg(4, typ)
	b.bool(5, cycle)
	return []byte(b)
}

type variant struct {
	body []byte
	slug string
}

func encodeVariants(id, disp string, tip []byte, contexts []contextChoice, efforts []effortChoice) []variant {
	def := defaultEffort(efforts)
	axes := efforts
	if len(axes) == 0 {
		axes = []effortChoice{{}}
	}
	ctxs := contexts
	if len(ctxs) == 0 {
		ctxs = []contextChoice{{}}
	}
	var out []variant
	for _, ctx := range ctxs {
		for _, effort := range axes {
			for _, fast := range fastChoices {
				out = append(out, encodeVariant(id, disp, tip, ctx, effort, fast, def))
			}
		}
	}
	return out
}

func encodeVariant(id, disp string, tip []byte, ctx contextChoice, effort effortChoice, fast bool, defEffort string) variant {
	var suffix []string
	if ctx.value != "" && ctx.value != defaultContext {
		suffix = append(suffix, ctx.label)
	}
	if effort.value != "" {
		suffix = append(suffix, effort.label)
	}
	if fast {
		suffix = append(suffix, "Fast")
	}
	shown := disp
	if len(suffix) > 0 {
		shown = disp + ` <span style="color: var(--cursor-text-tertiary);">` + strings.Join(suffix, " ") + `</span>`
	}
	// An empty context is the known-window case: there is no context
	// parameter, so that variant is the default context.
	contextDefault := ctx.value == "" || ctx.value == defaultContext
	isDef := contextDefault && !fast && effort.value == defEffort
	var b wbuf
	repr := id + "["
	slug := id
	sep := ""
	if ctx.value != "" {
		b.msg(1, encodeParamValue("context", ctx.value))
		repr += "context=" + ctx.value
		sep = ","
		slug += "-" + ctx.value
	}
	if effort.value != "" {
		b.msg(1, encodeParamValue("reasoning", effort.value))
		repr += sep + "reasoning=" + effort.value
		sep = ","
		slug += "-" + effort.value
	}
	fastWord := "false"
	if fast {
		fastWord = "true"
		slug += "-fast"
	}
	b.msg(1, encodeParamValue("fast", fastWord))
	repr += sep + "fast=" + fastWord + "]"
	b.str(2, shown)
	if isDef {
		b.bool(4, true)
		b.bool(5, true)
	}
	b.msg(6, tip)
	b.str(7, pickerSubtitle)
	b.str(8, shown)
	b.str(9, repr)
	b.str(11, slug)
	return variant{body: []byte(b), slug: slug}
}

func encodeParamValue(id, value string) []byte {
	var b wbuf
	b.str(1, id)
	b.str(2, value)
	return []byte(b)
}

func parseTokenCount(value string) (int, bool) {
	v := strings.TrimSpace(strings.ToLower(value))
	if v == "" {
		return 0, false
	}
	mult := 1
	switch v[len(v)-1] {
	case 'k':
		mult = 1_000
		v = v[:len(v)-1]
	case 'm':
		mult = 1_000_000
		v = v[:len(v)-1]
	}
	if v == "" {
		return 0, false
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n < 0 {
			return 0, false
		}
	}
	if n == 0 || n > (int(^uint(0)>>1))/mult {
		return 0, false
	}
	return n * mult, true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d [20]byte
	i := len(d)
	for n > 0 {
		i--
		d[i] = byte('0' + n%10)
		n /= 10
	}
	return string(d[i:])
}
