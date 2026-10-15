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
	if h.Initialization != nil {
		h.Initialization.Wake()
	}
}

func (h *Handler) handleInitializationAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("account_id")
	if h.Store.Initialization(id) == nil {
		writeAPIError(w, errNotFound("该账号没有初始化任务"))
		return
	}
	var payload struct {
		Revision          uint64 `json:"revision"`
		UseCurrentDefault bool   `json:"use_current_default"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeAPIError(w, errBadRequest("任务操作格式无效"))
		return
	}
	action := r.PathValue("action")
	var err error
	if action == "bind-default" {
		_, err = h.Store.BindInitialization(id, payload.Revision, payload.UseCurrentDefault)
	} else {
		err = h.Store.ChangeInitialization(id, payload.Revision, action)
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrInitializationConflict) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "initialization": h.Store.FindAny(id).Initialization})
	if h.Initialization != nil {
		h.Initialization.Wake()
	}
}
