package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"zcode2api/internal/store"
)

func (h *Handler) handleGetNewAccountPolicy(w http.ResponseWriter, r *http.Request) {
	policy, err := h.Store.NewAccountProxyPolicy()
	if err != nil {
		writeError500(w, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (h *Handler) handleSaveNewAccountPolicy(w http.ResponseWriter, r *http.Request) {
	policy := store.DefaultNewAccountProxyPolicy()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		writeAPIError(w, errBadRequest("初始化代理配置格式无效"))
		return
	}
	saved, err := h.Store.SaveNewAccountProxyPolicy(policy)
	if err != nil {
		if errors.Is(err, store.ErrInitializationConflict) {
			writeJSON(w, http.StatusConflict, map[string]any{"detail": err.Error()})
			return
		}
		writeAPIError(w, errBadRequest(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, saved)
}
