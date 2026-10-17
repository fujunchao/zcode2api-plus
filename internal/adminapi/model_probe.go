package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"zcode2api/internal/store"
)

func (h *Handler) handleModelProbeSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, h.Store.ModelProbeSettings())
		return
	}
	p := h.Store.ModelProbeSettings()
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		writeAPIError(w, errBadRequest("模型巡检配置格式无效"))
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeAPIError(w, errBadRequest("模型巡检配置只能包含一个对象"))
		return
	}
	saved, err := h.Store.SaveModelProbeSettings(p, time.Now())
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrModelProbeConflict) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, saved)
	if h.ModelProbe != nil {
		h.ModelProbe.Wake()
	}
}

func (h *Handler) handleModelProbeSummary(w http.ResponseWriter, r *http.Request) {
	result, err := h.Store.ModelProbeSummary(time.Now())
	if err != nil {
		writeAPIError(w, &apiError{status: http.StatusInternalServerError, message: "无法读取模型巡检状态"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) handleHealthSummary(w http.ResponseWriter, r *http.Request) {
	model, err := h.Store.ModelProbeSummary(time.Now())
	if err != nil {
		writeError500(w, err)
		return
	}
	network, err := h.Store.NetworkHealth()
	if err != nil {
		writeError500(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": model, "network": network, "soft": h.Store.ProxySoftFailures()})
}

func (h *Handler) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	if h.ModelProbe == nil {
		writeAPIError(w, errBadRequest("巡检管理器未就绪"))
		return
	}
	if err := h.ModelProbe.RequestHealth(r.PathValue("profile_id")); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"profile_id": r.PathValue("profile_id"), "state": "queued"})
}

func (h *Handler) handleStartModelProbe(w http.ResponseWriter, r *http.Request) {
	if h.ModelProbe == nil {
		writeAPIError(w, errBadRequest("模型巡检未初始化"))
		return
	}
	run, err := h.ModelProbe.Request(r.PathValue("profile_id"))
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (h *Handler) handleModelProbeHistory(w http.ResponseWriter, r *http.Request) {
	offset, limit := 0, 50
	for key, out := range map[string]*int{"offset": &offset, "limit": &limit} {
		if value := r.URL.Query().Get(key); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 || (key == "limit" && (n < 1 || n > 100)) {
				writeAPIError(w, errBadRequest("分页范围无效"))
				return
			}
			*out = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": h.Store.ModelProbeHistory(r.PathValue("profile_id"), offset, limit), "offset": offset, "limit": limit})
}
