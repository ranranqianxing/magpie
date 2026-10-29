// Package cursoride publishes magpie's models into the ordinary Cursor IDE.
//
// Cursor Private Inference (internal/agent/cursorlocal.go) is a different
// product. That build reads the gateway from CURSOR_LOCAL_AGENT_BASE_URL
// and CURSOR_LOCAL_AGENT_API_KEY. The IDE everyone else runs asks Cursor's
// backend for AvailableModels and names the chosen model on its inference
// protocol. This package builds those AvailableModels rows from magpie's
// catalog and turns the id Cursor sends back into a chat request the
// gateway already serves.
//
// It does not listen, write Cursor's settings or state.vscdb, or set the
// Private Inference variables. A proxy in front of Cursor calls Handler
// for the catalog and Route plus CompileChat for a run. See
// docs/subsystems/cursor-ide.md.
package cursoride
