// image_handler.go /v1/images/generations：pool 挑号 → 4299 提交+轮询 → OpenAI 格式。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"qclaw2api/internal/upstream"
)

// imageRequest OpenAI 生图请求体（仅支持 n=1；size 非默认忽略）。
type imageRequest struct {
	Prompt string `json:"prompt"`
	N      int    `json:"n"`
	Size   string `json:"size"`
}

// isImageTimeout 报告生图轮询是否超时（pending 正常排队，不冷却账号）。
func isImageTimeout(err error) bool {
	return errors.Is(err, upstream.ErrImageTimeout)
}

// errRateLimited 本地 rpm 超限错误。
var errRateLimited = errors.New("rpm limit exceeded locally")

// images 处理生图请求。
func (h *Handler) images(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req imageRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "prompt is required")
		return
	}
	if req.N != 0 && req.N != 1 {
		writeOpenAIError(w, http.StatusBadRequest, "unsupported", "n>1 not supported (4299 single image)")
		return
	}

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UserID] = true

		if !h.cfg.Pool.ReserveToken(acct.UserID) {
			lastErr = errRateLimited
			continue
		}
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.refreshAccount(r.Context(), acct); err != nil {
				lastErr = err
				continue
			}
		}

		url, err := h.cfg.Upstream.GenerateImage(r.Context(), acct, req.Prompt)
		if err != nil {
			// 生图轮询超时（pending 正常排队）不冷却账号（SPEC §3.6）
			if isImageTimeout(err) {
				writeOpenAIError(w, http.StatusGatewayTimeout, "image_timeout", err.Error())
				return
			}
			lastErr = err
			h.cfg.Pool.NoteError(acct.UserID)
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UserID)
		writeJSON(w, http.StatusOK, map[string]any{
			"created": time.Now().Unix(),
			"data":    []any{map[string]any{"url": url}},
		})
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}
