package adminapi

import (
	"errors"
	"net/http"
	"strings"

	"zcode2api/internal/store"
)

func (h *Handler) handleStrictProxyStatus(w http.ResponseWriter, r *http.Request) {
	status, err := h.Store.StrictProxySummary()
	if err != nil {
		writeAPIError(w, &apiError{status: http.StatusInternalServerError, message: "无法读取严格代理状态"})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// 恢复的是尚未提交的删除事务，不是把被删代理复活，也不访问真实模型端点。
func (h *Handler) handleRecoverStrictProxy(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.RecoverStrictProxyRetirements(); err != nil {
		writeAPIError(w, &apiError{status: http.StatusInternalServerError, message: "严格删除事务仍未恢复，已保留禁止派发状态"})
		return
	}
	h.handleStrictProxyStatus(w, r)
}

// 人工解除要求明确确认当前决定及独立复核；不把连通性探测成功冒充模型根因验证。
func (h *Handler) handleUnblockStrictProxy(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	decision, _ := payload["decision_id"].(string)
	revalidated, _ := payload["revalidated"].(bool)
	if strings.TrimSpace(decision) == "" || !revalidated {
		writeAPIError(w, errBadRequest("需提供当前 decision_id 并明确声明已完成独立复核（revalidated=true）"))
		return
	}
	if err := h.Store.UnblockStrictProxy(r.PathValue("identity"), decision); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeAPIError(w, errNotFound("严格淘汰记录不存在"))
		case errors.Is(err, store.ErrStrictDecisionConflict):
			writeAPIError(w, &apiError{status: http.StatusConflict, message: "淘汰决定已变化，请重新查看并复核"})
		default:
			writeAPIError(w, &apiError{status: http.StatusInternalServerError, message: "未解除禁止记录，请先检查删除事务及持久化状态"})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restored": false, "requires_import": true})
}
