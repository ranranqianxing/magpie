package cursoride

import (
	"fmt"
	"io"
	"strings"
	"bytes"
	"os"
	"net/http"
)

// Cursor's catalog paths. Inference streams are not served here: the proxy
// reads those frames and calls Decide and CompileChat.
const (
	PathAvailableModels   = "/aiserver.v1.AiService/AvailableModels"
	PathUsableModels      = "/agent.v1.AgentService/GetUsableModels"
	PathUsableModelsAI    = "/aiserver.v1.AiService/GetUsableModels"
	PathDefaultModel      = "/aiserver.v1.AiService/GetDefaultModel"
	PathDefaultModelCLI   = "/aiserver.v1.AiService/GetDefaultModelForCli"
	PathDefaultModelNudge = "/aiserver.v1.AiService/GetDefaultModelNudgeData"
)

// maxCatalogBody is how much of a catalog request is read. Past that the
// handler refuses the call. It does not pretend the body was empty and
// answer with magpie's models alone, which would drop Cursor's own list.
const maxCatalogBody = 32 << 20

// Upstream fetches Cursor's own reply for the same catalog request.
// status outside 2xx, or a non-nil err, is "Cursor's backend did not
// answer": the handler then returns magpie's rows alone. contentType is
// sent back with a merged body so a framed reply keeps the type Cursor
// used. Nil Upstream means the backend was not asked.
type Upstream func(r *http.Request, body []byte) (status int, resp []byte, contentType string, err error)

// Handler answers Cursor's model-list calls. Models is read on each
// request, so a provider added in magpie shows up on Cursor's next list
// without a restart of this process. Nil Models is an empty catalog.
// MaxBody, when set, replaces maxCatalogBody. A test uses it; a proxy
// leaves it 0.
type Handler struct {
	Models   func() []Model
	Upstream Upstream
	MaxBody  int
}

// LiveHandler serves whatever magpie's agent catalog lists now.
func LiveHandler() *Handler {
	return &Handler{Models: LiveCatalog}
}

func (h *Handler) models() []Model {
	if h == nil || h.Models == nil {
		return nil
	}
	return h.Models()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	limit := maxCatalogBody
	if h != nil && h.MaxBody > 0 {
		limit = h.MaxBody
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	if len(body) > limit {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	models := h.models()
	switch r.URL.Path {
	case PathAvailableModels:
		h.merge(w, r, body, AvailableModels(models))
	case PathUsableModels, PathUsableModelsAI:
		h.merge(w, r, body, UsableModels(models))
	case PathDefaultModel:
		writeProto(w, DefaultModel(models), "application/proto")
	case PathDefaultModelCLI:
		writeProto(w, DefaultModelForCLI(models), "application/proto")
	case PathDefaultModelNudge:
		writeProto(w, DefaultModelNudge(models), "application/proto")
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) merge(w http.ResponseWriter, r *http.Request, body, extra []byte) {
	logf := func(format string, args ...any) {
		if f, err := os.OpenFile("/tmp/magpie-merge.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			fmt.Fprintf(f, format, args...)
			f.Close()
		}
	}
	if h.Upstream == nil {
		logf("path=%s branch=no-upstream extra=%d hasDS=%v\n", r.URL.Path, len(extra), bytes.Contains(extra, []byte("deepseek/")))
		writeProto(w, extra, "application/proto")
		return
	}
	status, up, ctype, err := h.Upstream(r, body)
	if err != nil || status < 200 || status >= 300 {
		logf("path=%s branch=upstream-fail status=%d err=%v up=%d extra=%d hasDS=%v\n", r.URL.Path, status, err, len(up), len(extra), bytes.Contains(extra, []byte("deepseek/")))
		_ = os.WriteFile("/tmp/magpie-up-fail-"+strings.ReplaceAll(r.URL.Path, "/", "_")+".bin", extra, 0644)
		writeProto(w, extra, "application/proto")
		return
	}
	merged, err := MergeUnary(up, extra)
	if err != nil {
		logf("path=%s branch=merge-fail err=%v up=%d extra=%d\n", r.URL.Path, err, len(up), len(extra))
		_ = os.WriteFile("/tmp/magpie-merge-fail-up.bin", up, 0644)
		_ = os.WriteFile("/tmp/magpie-merge-fail-extra.bin", extra, 0644)
		http.Error(w, "catalog", http.StatusBadGateway)
		return
	}
	logf("path=%s branch=merged status=%d up=%d extra=%d merged=%d hasDS=%v ctype=%s\n", r.URL.Path, status, len(up), len(extra), len(merged), bytes.Contains(merged, []byte("deepseek/")), ctype)
	if strings.Contains(r.URL.Path, "AvailableModels") {
		_ = os.WriteFile("/tmp/magpie-merged-available.bin", merged, 0644)
		_ = os.WriteFile("/tmp/magpie-up-available.bin", up, 0644)
	}
	if ctype == "" {
		ctype = "application/proto"
	}
	writeProto(w, merged, ctype)
}

func writeProto(w http.ResponseWriter, body []byte, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
