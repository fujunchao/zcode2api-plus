package asyncpool

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"zcode2api/internal/config"
)

func TestTicketTimeoutIsCountedAsFailedRequest(t *testing.T) {
	pool, st, _, _ := newTestPool(t)
	ticket := insertTicket(pool, "expired", map[string]any{})
	ticket.createdAt = time.Now().Add(-time.Duration(config.AsyncTicketTimeout+1) * time.Second)
	handler := st.Requests.Wrap(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		pool.streamTicket(r.Context(), func(s string) error { _, err := io.WriteString(w, s); return err }, "expired")
	})
	handler(httptest.NewRecorder(), httptest.NewRequest("POST", "/async/v1/messages", nil))
	got := st.Requests.Snapshot()
	if got.Total != 1 || got.Succeeded != 0 || got.Failed != 1 || got.Active != 0 || got.UpstreamAttempts != 0 {
		t.Fatalf("票务超时被误计为成功：%+v", got)
	}
}
