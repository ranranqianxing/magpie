package cursoride

import (
	"strings"

	"github.com/yetone/magpie/internal/provider"
)

// LiveCatalog is magpie's current agent catalog as Cursor rows.
func LiveCatalog() []Model { return FromEntries(provider.Catalog()) }

// FromEntries copies catalog entries into Cursor rows.
//
// The display name is the model's own name (the user's rename when they
// gave one). It is not Entry.Label, which appends the provider, and a
// provider the user added by its address is often named by that host.
// The provider's key and base URL are not read.
func FromEntries(es []provider.Entry) []Model {
	out := make([]Model, 0, len(es))
	for _, e := range es {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(e.Name)
		if name == "" {
			name = strings.TrimSpace(e.Model)
		}
		if name == "" {
			name = id
		}
		out = append(out, Model{
			ID: id, Name: name, Reasoning: e.Thinks(),
			Efforts: append([]string(nil), e.Efforts...),
			Images:  e.Images, Context: e.Context,
		})
	}
	return out
}
