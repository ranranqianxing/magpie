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
// Proxy listens on loopback and terminates TLS for Cursor's hosts. The
// Agents-page Cursor row (internal/agent/cursoride.go) points the IDE's
// http.proxy there and stashes the previous values. This package does not
// write state.vscdb, Cursor's Open configuration, or the Private Inference
// variables, and it does not install its CA into the system trust store.
// See docs/subsystems/cursor-ide.md.
package cursoride
