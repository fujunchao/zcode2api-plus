package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"zcode2api/internal/proxysub"
)

func (h *Handler) handleSubscription(w http.ResponseWriter, r *http.Request) {
	view, err := h.Subscription.View()
	if err != nil {
		writeAPIError(w, &apiError{status: 500, message: "无法读取订阅状态"})
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) handleSaveSubscription(w http.ResponseWriter, r *http.Request) {
	payload, e := decodeBody(r)
	if e != nil {
		writeAPIError(w, e)
		return
	}
	snap, err := h.Store.SubscriptionSnapshot()
	if err != nil {
		writeAPIError(w, &apiError{status: 500, message: "无法读取订阅状态"})
		return
	}
	if _, ok := payload["revision"]; !ok {
		writeAPIError(w, errBadRequest("缺少配置 revision，请刷新"))
		return
	}
	allowed := map[string]bool{"revision": true, "url": true, "clear_url": true, "auto": true, "interval_minutes": true,
		"max_age_minutes": true, "candidates": true, "import_limit": true, "capacity": true, "concurrency": true, "budget_seconds": true, "enable_imported": true}
	for key := range payload {
		if !allowed[key] {
			writeAPIError(w, errBadRequest("未知订阅配置字段"))
			return
		}
	}
	c := snap.State.Config
	raw, _ := json.Marshal(payload)
	if json.Unmarshal(raw, &c) != nil {
		writeAPIError(w, errBadRequest("订阅配置字段类型无效"))
		return
	}
	clear := false
	if value, ok := payload["clear_url"]; ok {
		var valid bool
		clear, valid = value.(bool)
		if !valid {
			writeAPIError(w, errBadRequest("clear_url 必须为布尔值"))
			return
		}
	}
	if value, ok := payload["url"]; ok {
		url, valid := value.(string)
		if !valid || url == "" || clear {
			writeAPIError(w, errBadRequest("替换地址需提供非空字符串；清空请单独使用 clear_url"))
			return
		}
	}
	if clear {
		c.URL = ""
		c.Auto = false
	}
	c, err = h.Subscription.Configure(c)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, proxysub.ErrConflict) {
			status = http.StatusConflict
		}
		if errors.Is(err, proxysub.ErrState) {
			status = http.StatusInternalServerError
		}
		writeAPIError(w, &apiError{status: status, message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, c.Public())
}

func (h *Handler) handleStartSubscription(w http.ResponseWriter, r *http.Request) {
	payload, e := decodeBody(r)
	if e != nil {
		writeAPIError(w, e)
		return
	}
	mode, _ := payload["mode"].(string)
	run, err := h.Subscription.Launch(mode, false)
	if errors.Is(err, proxysub.ErrBusy) {
		writeJSON(w, http.StatusConflict, map[string]any{"detail": err.Error(), "current": run})
		return
	}
	if err != nil {
		writeAPIError(w, errBadRequest(err.Error()))
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (h *Handler) handleSubscriptionRuns(w http.ResponseWriter, r *http.Request) {
	view, err := h.Subscription.View()
	if err != nil {
		writeAPIError(w, &apiError{status: 500, message: "无法读取订阅任务"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": view["runs"], "current": view["current"], "last_result": view["last_result"]})
}

func (h *Handler) handleSubscriptionRun(w http.ResponseWriter, r *http.Request) {
	view, err := h.Subscription.View()
	if err != nil {
		writeAPIError(w, &apiError{status: 500, message: "无法读取订阅任务"})
		return
	}
	id := r.PathValue("run_id")
	for _, key := range []string{"current", "last_result"} {
		if run, ok := view[key].(*proxysub.Run); ok && run != nil && run.ID == id {
			writeJSON(w, 200, run)
			return
		}
	}
	for _, run := range view["runs"].([]proxysub.Run) {
		if run.ID == id {
			writeJSON(w, 200, run)
			return
		}
	}
	writeAPIError(w, errNotFound("任务不存在或已超过保留上限"))
}

func (h *Handler) handleCancelSubscription(w http.ResponseWriter, r *http.Request) {
	if !h.Subscription.Cancel(r.PathValue("run_id")) {
		writeAPIError(w, errNotFound("任务不存在"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleSubscriptionBlocks(w http.ResponseWriter, r *http.Request) {
	snap, err := h.Store.SubscriptionSnapshot()
	if err != nil {
		writeAPIError(w, &apiError{status: 500, message: "无法读取暂缓记录"})
		return
	}
	blocks := []proxysub.Block{}
	for _, b := range snap.State.Blocks {
		if b.Active(time.Now()) {
			blocks = append(blocks, b)
		}
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].CreatedAt > blocks[j].CreatedAt })
	writeJSON(w, 200, map[string]any{"blocks": blocks, "import_paused": snap.State.ImportPaused})
}

func (h *Handler) handleUnblockSubscription(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.UnblockSubscription(r.PathValue("fingerprint")); err != nil {
		if errors.Is(err, proxysub.ErrState) {
			writeAPIError(w, &apiError{status: 500, message: "无法保存暂缓记录"})
			return
		}
		writeAPIError(w, errNotFound("暂缓记录不存在"))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
