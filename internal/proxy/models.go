package proxy

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"router/internal/openai"
)

// startupTime is used as the "created" timestamp for every public model,
// since public model names are router-defined and have no real creation
// date to report — matching OpenAI's schema without inventing upstream
// provenance.
var startupTime = time.Now().Unix()

// Models serves GET /v1/models, listing only the public model names —
// upstream identities never appear here.
func (h *Handlers) Models(w http.ResponseWriter, r *http.Request) {
	cfg := h.Registry.Config()
	list := openai.ModelList{Object: "list"}
	for _, pm := range cfg.PublicModels {
		list.Data = append(list.Data, openai.Model{
			ID:      pm.Name,
			Object:  "model",
			Created: startupTime,
			OwnedBy: "router",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

// ModelByID serves GET /v1/models/{id}.
func (h *Handlers) ModelByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	cfg := h.Registry.Config()
	if _, ok := cfg.PublicModelByName(id); !ok {
		writeFatal(w, http.StatusNotFound, "The model '"+id+"' does not exist", openai.ErrTypeNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(openai.Model{
		ID:      id,
		Object:  "model",
		Created: startupTime,
		OwnedBy: "router",
	})
}

// Healthz is a liveness probe: no upstream checks, just "the process is up".
func (h *Handlers) Healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
