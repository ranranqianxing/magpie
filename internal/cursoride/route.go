package cursoride

import (
	"fmt"
	"strings"
)

// AgentID is the gateway caller a proxy should use for a turn it compiled
// (gateway.TokenFor). It is not Cursor Private Inference's "cursor-local".
const AgentID = "cursor-ide"

// Parameter is one model parameter Cursor sent with the route id.
// Ids this package publishes are "context", "reasoning" and "fast".
// "effort" and "reasoning_effort" are accepted as the same as "reasoning".
type Parameter struct {
	ID    string
	Value string
}

// Route is a Cursor selection resolved to a magpie model.
//
// Model is the catalog id, sent as the chat body's "model". Effort is the
// level Cursor asked for, "" when it named none and "none" when it asked
// for none. ContextTokens and Fast are what the picker sent. This slice's
// chat body carries Model and Effort; ContextTokens and Fast stay on the
// route for the proxy that will honor them.
type Route struct {
	Model         string
	DisplayName   string
	Effort        string
	ContextTokens int
	Fast          bool
}

// Route resolves modelID against models. ok is false when modelID is not
// one of them, including Cursor's own models, which the proxy forwards.
// A variant string ("id[context=200k,reasoning=high,fast=false]") and a
// legacy slug ("id-200k-high-fast") resolve to the same model. params
// win over either form. An unknown id with a bad parameter is not an
// error: it is not ours. A known id with a context or fast value that
// does not parse is an error, not a guess.
func RouteModel(models []Model, modelID string, params []Parameter) (Route, bool, error) {
	models = publishable(models)
	modelID = strings.TrimSpace(modelID)
	if modelID == "" || len(models) == 0 {
		return Route{}, false, nil
	}
	byID := make(map[string]Model, len(models))
	for _, m := range models {
		byID[m.ID] = m
	}
	if i := strings.IndexByte(modelID, '['); i > 0 {
		if m, found := byID[modelID[:i]]; found {
			base, embedded, ok := splitVariant(modelID)
			if !ok || base != m.ID {
				return Route{}, false, errf("invalid variant %q", modelID)
			}
			return finish(m, overlay(embedded, params))
		}
	}
	if m, found := byID[modelID]; found {
		return finish(m, params)
	}
	if id, embedded, ok := legacySlug(models, modelID); ok {
		return finish(byID[id], overlay(embedded, params))
	}
	return Route{}, false, nil
}

func finish(m Model, params []Parameter) (Route, bool, error) {
	r := Route{Model: m.ID, DisplayName: m.Name}
	for _, p := range params {
		switch strings.ToLower(strings.TrimSpace(p.ID)) {
		case "effort", "reasoning", "reasoning_effort":
			e := strings.ToLower(strings.TrimSpace(p.Value))
			if e == "extra-high" {
				e = "xhigh"
			}
			if e == "" {
				continue
			}
			r.Effort = e
		case "context", "context_window":
			n, ok := parseTokenCount(p.Value)
			if !ok {
				return Route{}, false, errf("invalid context %q", strings.TrimSpace(p.Value))
			}
			r.ContextTokens = n
		case "fast":
			switch strings.ToLower(strings.TrimSpace(p.Value)) {
			case "true":
				r.Fast = true
			case "false":
				r.Fast = false
			default:
				return Route{}, false, errf("invalid fast %q", strings.TrimSpace(p.Value))
			}
		}
	}
	return r, true, nil
}

func splitVariant(modelID string) (base string, params []Parameter, ok bool) {
	i := strings.IndexByte(modelID, '[')
	if i <= 0 || !strings.HasSuffix(modelID, "]") || strings.Contains(modelID[i+1:], "[") {
		return "", nil, false
	}
	inner := modelID[i+1 : len(modelID)-1]
	base = modelID[:i]
	if inner == "" {
		return base, nil, true
	}
	for _, pair := range strings.Split(inner, ",") {
		k, v, found := strings.Cut(pair, "=")
		if !found || k == "" {
			return "", nil, false
		}
		params = append(params, Parameter{ID: k, Value: v})
	}
	return base, params, true
}

func legacySlug(models []Model, modelID string) (string, []Parameter, bool) {
	var best string
	var params []Parameter
	for _, m := range models {
		p, ok := slugParams(m.ID, modelID, effortChoices(m))
		if !ok {
			continue
		}
		if len(m.ID) > len(best) {
			best, params = m.ID, p
		}
	}
	if best == "" {
		return "", nil, false
	}
	return best, params, true
}

func slugParams(id, modelID string, efforts []effortChoice) ([]Parameter, bool) {
	rest, ok := strings.CutPrefix(modelID, id+"-")
	if !ok || rest == "" || strings.Contains(rest, "[") {
		return nil, false
	}
	parts := strings.Split(rest, "-")
	if len(parts) < 1 || len(parts) > 3 {
		return nil, false
	}
	if _, ok := parseTokenCount(parts[0]); !ok {
		return nil, false
	}
	params := []Parameter{{ID: "context", Value: parts[0]}}
	switch len(parts) {
	case 1:
		return params, true
	case 2:
		if parts[1] == "fast" {
			return append(params, Parameter{ID: "fast", Value: "true"}), true
		}
		if !effortListed(parts[1], efforts) {
			return nil, false
		}
		return append(params, Parameter{ID: "reasoning", Value: parts[1]}), true
	default:
		if !effortListed(parts[1], efforts) || parts[2] != "fast" {
			return nil, false
		}
		return append(params,
			Parameter{ID: "reasoning", Value: parts[1]},
			Parameter{ID: "fast", Value: "true"}), true
	}
}

func effortListed(s string, efforts []effortChoice) bool {
	for _, e := range efforts {
		if e.value == s {
			return true
		}
	}
	return false
}

// overlay lets params replace embedded ones with the same id. params are
// what the request said on their own, so they win over a variant string.
func overlay(embedded, params []Parameter) []Parameter {
	if len(params) == 0 {
		return embedded
	}
	out := append([]Parameter(nil), embedded...)
	for _, p := range params {
		replaced := false
		for i := range out {
			if strings.EqualFold(out[i].ID, p.ID) {
				out[i] = p
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, p)
		}
	}
	return out
}

func errf(format string, args ...any) error {
	return fmt.Errorf("cursor ide: "+format, args...)
}
