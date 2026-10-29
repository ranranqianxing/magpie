package cursoride

import "strings"

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
// that same id back on the inference request. Name is the black picker
// text (client_display_name). It is the model's name, never a provider
// host and never a routing-group label stuffed into a badge.
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
	// Context is the prompt window in tokens, when known. A size the
	// fixed list does not already name is one more context choice.
	Context int
}

type effortChoice struct{ value, label string }

type contextChoice struct{ value, label string }

const defaultContext = "200k"

// contextPresets are the context sizes Cursor's picker is given for a
// magpie model. The words are the parameter values Cursor sends back.
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
	models = publishable(models)
	var b wbuf
	for _, m := range models {
		b.str(1, m.ID)
	}
	for _, m := range models {
		b.msg(2, encodeModel(m))
	}
	return []byte(b)
}

// UsableModels is the protobuf appended to GetUsableModels. Each row is
// agent.v1.ModelDetails: the route id, the display name, and the local
// credential marker. Both of Cursor's usable-model paths take this shape.
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

func encodeModel(m Model) []byte {
	disp := m.Name
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
	b.u32(38, 1)
	b.str(41, vendorLabel)
	b.msg(42, encodeVendor())
	return []byte(b)
}

// vendorLabel is the vendor line on every injected row. One word, not
// each provider's name: a custom provider is often named by its host,
// and that host is what Cursor's badges were painting in blue.
const vendorLabel = "magpie"

func encodeVendor() []byte {
	var b wbuf
	b.u32(1, 6) // Cursor's MODEL_VENDOR_ID_CURSOR, the slot these rows use
	b.str(2, vendorLabel)
	return []byte(b)
}

func encodeTooltip(markdown string) []byte {
	var b wbuf
	b.str(7, markdown)
	return []byte(b)
}

func encodeUsable(m Model) []byte {
	var cred wbuf
	cred.str(1, LocalCredential)
	var b wbuf
	b.str(1, m.ID)
	b.msg(2, nil) // thinking_details present, with nothing else in it
	b.str(3, m.ID)
	b.str(4, m.Name)
	b.str(5, m.Name)
	b.msg(8, cred)
	return []byte(b)
}

func contextChoices(tokens int) []contextChoice {
	out := append([]contextChoice(nil), contextPresets...)
	if tokens <= 0 {
		return out
	}
	for _, c := range out {
		n, ok := parseTokenCount(c.value)
		if ok && n == tokens {
			return out
		}
	}
	out = append(out, contextChoice{value: itoa(tokens), label: formatTokenCount(tokens) + " (Custom)"})
	return out
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
	out := [][]byte{encodeEnumParam("context", "Context",
		"Context size used to trigger conversation compaction.", contexts, false)}
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
	var out []variant
	for _, ctx := range contexts {
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
	if ctx.value != defaultContext {
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
	isDef := ctx.value == defaultContext && !fast && effort.value == defEffort
	var b wbuf
	b.msg(1, encodeParamValue("context", ctx.value))
	repr := id + "[context=" + ctx.value
	slug := id + "-" + ctx.value
	if effort.value != "" {
		b.msg(1, encodeParamValue("reasoning", effort.value))
		repr += ",reasoning=" + effort.value
		slug += "-" + effort.value
	}
	fastWord := "false"
	if fast {
		fastWord = "true"
		slug += "-fast"
	}
	b.msg(1, encodeParamValue("fast", fastWord))
	repr += ",fast=" + fastWord + "]"
	b.str(2, shown)
	if isDef {
		b.bool(4, true)
		b.bool(5, true)
	}
	b.msg(6, tip)
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

func formatTokenCount(n int) string {
	if n >= 1_000_000 && n%1_000_000 == 0 {
		return itoa(n/1_000_000) + "M"
	}
	if n >= 1_000 && n%1_000 == 0 {
		return itoa(n/1_000) + "K"
	}
	return itoa(n)
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
