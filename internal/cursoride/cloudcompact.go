package cursoride

import "strings"

// useCloudAgentEffortModes is AvailableModelsRequest field 12.
// The Cloud Agents window sets it. The IDE agent picker does not.
const useCloudAgentEffortModes = 12

// Automations asks AvailableModels on its own call, then drops a row
// whose visible_in_routed_model_view is set (excludeRoutedModels),
// aside from the Auto names. The workbench and the molten engine both
// send scope AUTOMATIONS together with use_parameterized_automations_models.
// for_automations is the same call when a client sets that field.
// The compact pickers do not send any of these.
const (
	fieldForAutomations           = 9
	fieldModelsScope              = 10
	fieldParameterizedAutomations = 15
	scopeAutomations              = 2
)

// Routed-view fields on AvailableModel. The compact list keeps a row
// only when visible_in_routed_model_view and default_on are both set.
const (
	fieldDefaultOn        = 2
	fieldDegradation      = 6
	fieldRecommendedCloud = 21
	fieldOnlyCmdK         = 27
	fieldCloudSort        = 28
	fieldCloudEffort      = 32
	fieldCloudMigrate     = 33
	fieldHidden           = 35
	fieldNamedSection     = 38
	fieldRoutedView       = 40
	fieldVendor           = 42
	fieldCloudEffortModes = 44
	degradationDisabled   = 2
	modelVendorCursor     = 6
)

// autoModelIDs are the picker rows that stand for Auto. The compact
// list draws Auto on its own toggle. Marking one of these routed would
// put a second Auto row beside the models.
var autoModelIDs = map[string]bool{
	"auto":        true,
	"auto-high":   true,
	"auto-low":    true,
	"auto-medium": true,
	"auto-smart":  true,
	"default":     true,
	"premium":     true,
}

// cloudCatalogRequest reports whether body is the Cloud Agents window's
// AvailableModels call. A framed body is read as its payload. A body
// this package cannot parse is not that call.
func cloudCatalogRequest(body []byte) bool {
	payload, _, err := UnwrapUnary(body)
	if err != nil {
		return false
	}
	fs, err := parseFields(payload)
	if err != nil {
		return false
	}
	v, ok := lastVarint(fs, useCloudAgentEffortModes)
	return ok && v == 1
}

// automationsCatalogRequest reports whether body is an Automations
// AvailableModels call. A framed body is read as its payload. A body
// this package cannot parse is not that call: a failed read is
// unknown, and the compact lists are the ones that need the routed flag.
func automationsCatalogRequest(body []byte) bool {
	payload, _, err := UnwrapUnary(body)
	if err != nil {
		return false
	}
	fs, err := parseFields(payload)
	if err != nil {
		return false
	}
	if v, ok := lastVarint(fs, fieldForAutomations); ok && v == 1 {
		return true
	}
	if v, ok := lastVarint(fs, fieldModelsScope); ok && v == scopeAutomations {
		return true
	}
	v, ok := lastVarint(fs, fieldParameterizedAutomations)
	return ok && v == 1
}

// promoteCloudCompact sets visible_in_routed_model_view on the official
// rows the Cloud Agents compact list would otherwise drop.
//
// That window asks for use_cloud_agent_effort_modes and then opens the
// routed compact list, which keeps a row only when the flag and
// default_on are both set. Official cloud rows often omit the flag.
// Injected rows set it, so the list becomes those rows alone. The IDE
// catalog is a different request and is not rewritten, so a row Cursor
// left out of that compact list stays out.
//
// A body this package cannot parse is returned unchanged. A failed
// read is not an empty catalog.
func promoteCloudCompact(upstream []byte) []byte {
	payload, framed, err := UnwrapUnary(upstream)
	if err != nil {
		return upstream
	}
	next, ok := promoteCloudPayload(payload)
	if !ok {
		return upstream
	}
	if !framed {
		return next
	}
	out := make([]byte, 5+len(next))
	out[0] = upstream[0]
	putU32(out[1:5], uint32(len(next)))
	copy(out[5:], next)
	return out
}

func promoteCloudPayload(payload []byte) ([]byte, bool) {
	var out []byte
	changed := false
	i := 0
	for i < len(payload) {
		start := i
		key, n := takeVarint(payload[i:])
		if n == 0 || int(key>>3) <= 0 {
			return nil, false
		}
		i += n
		num := int(key >> 3)
		end, msg, ok := skipValue(payload, i, int(key&7))
		if !ok {
			return nil, false
		}
		i = end
		if num == 2 && msg != nil {
			if promoted, did := promoteModel(msg); did {
				changed = true
				var w wbuf
				w.msg(2, promoted)
				out = append(out, w...)
				continue
			}
		}
		out = append(out, payload[start:i]...)
	}
	if !changed {
		return nil, false
	}
	return out, true
}

// skipValue advances past one protobuf value starting at i. For a
// length-delimited value, msg is that value. ok is false when the
// buffer ends first.
func skipValue(b []byte, i, wt int) (end int, msg []byte, ok bool) {
	switch wt {
	case 0:
		_, n := takeVarint(b[i:])
		if n == 0 {
			return 0, nil, false
		}
		return i + n, nil, true
	case 1:
		if len(b[i:]) < 8 {
			return 0, nil, false
		}
		return i + 8, nil, true
	case 2:
		ln, n := takeVarint(b[i:])
		if n == 0 || uint64(len(b[i+n:])) < ln {
			return 0, nil, false
		}
		return i + n + int(ln), b[i+n : i+n+int(ln)], true
	case 5:
		if len(b[i:]) < 4 {
			return 0, nil, false
		}
		return i + 4, nil, true
	default:
		return 0, nil, false
	}
}

func promoteModel(msg []byte) ([]byte, bool) {
	fs, err := parseFields(msg)
	if err != nil || !cloudCompactOfficial(fs) {
		return msg, false
	}
	kept, ok := dropField(msg, fieldRoutedView)
	if !ok {
		return msg, false
	}
	var w wbuf
	w.bool(fieldRoutedView, true)
	return append(kept, w...), true
}

// cloudCompactOfficial is an upstream row the cloud compact list should
// keep. It is already default_on, and it is a named-section row, a
// cloud-effort row, a background-composer row, or a Cursor-vendor row.
// Auto, hidden, migrating, cmd-k-only, and disabled rows stay as Cursor
// sent them. A row whose flag is already set needs nothing.
func cloudCompactOfficial(fs []field) bool {
	if routedVisible(fs) {
		return false
	}
	name, ok := fieldString(fs, 1)
	if !ok || autoModelIDs[name] {
		return false
	}
	on, present := lastVarint(fs, fieldDefaultOn)
	if !present || on != 1 {
		return false
	}
	if v, ok := lastVarint(fs, fieldHidden); ok && v != 0 {
		return false
	}
	if v, ok := lastVarint(fs, fieldOnlyCmdK); ok && v != 0 {
		return false
	}
	if v, ok := lastVarint(fs, fieldDegradation); ok && v == degradationDisabled {
		return false
	}
	if s, ok := fieldString(fs, fieldCloudMigrate); ok && strings.TrimSpace(s) != "" {
		return false
	}
	if hasField(fs, fieldNamedSection) || cursorVendor(fs) {
		return true
	}
	if v, ok := lastVarint(fs, fieldRecommendedCloud); ok && v != 0 {
		return true
	}
	if hasField(fs, fieldCloudSort) || hasField(fs, fieldCloudEffortModes) {
		return true
	}
	v, ok := lastVarint(fs, fieldCloudEffort)
	return ok && v != 0
}

func routedVisible(fs []field) bool {
	v, ok := lastVarint(fs, fieldRoutedView)
	return ok && v != 0
}

func cursorVendor(fs []field) bool {
	for _, f := range fs {
		if f.num != fieldVendor || f.wt != 2 {
			continue
		}
		inner, err := parseFields(f.b)
		if err != nil {
			return false
		}
		v, ok := lastVarint(inner, 1)
		return ok && v == modelVendorCursor
	}
	return false
}

func lastVarint(fs []field, num int) (uint64, bool) {
	var v uint64
	ok := false
	for _, f := range fs {
		if f.num == num && f.wt == 0 {
			v = f.v
			ok = true
		}
	}
	return v, ok
}

// dropField copies msg without any occurrence of num. A truncated
// message returns ok false: the caller keeps the original bytes.
func dropField(msg []byte, num int) ([]byte, bool) {
	out := []byte{}
	i := 0
	for i < len(msg) {
		start := i
		key, n := takeVarint(msg[i:])
		if n == 0 || int(key>>3) <= 0 {
			return nil, false
		}
		i += n
		end, _, ok := skipValue(msg, i, int(key&7))
		if !ok {
			return nil, false
		}
		i = end
		if int(key>>3) == num {
			continue
		}
		out = append(out, msg[start:i]...)
	}
	return out, true
}

func putU32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}
