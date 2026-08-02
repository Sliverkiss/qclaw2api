// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + jprx/aizone 转发。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"qclaw2api/internal/auth"
	"qclaw2api/internal/jprx"
	"qclaw2api/internal/pool"
	"qclaw2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	JPRX         *jprx.Client
	APIKey       string        // 空 = 不鉴权
	MaxRotate    int           // 单请求最多换号次数，默认 3
	HardCooldown time.Duration // 余额不足/未激活冷却，默认 12h
	SoftCooldown time.Duration // 429/rpm 冷却，默认 60s
	ErrThreshold int           // 连续其他错误冷却阈值
	ErrCooldown  time.Duration // 错误冷却时长
	RefreshSkew  time.Duration // token 提前刷新窗口
	// AggregateTimeout 非流式聚合总超时（P1-5），默认 120s；上游挂死时按时返回错误。
	AggregateTimeout time.Duration
}

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	modelCache modelCache
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.HardCooldown <= 0 {
		cfg.HardCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 5
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 7 * 24 * time.Hour
	}
	if cfg.AggregateTimeout <= 0 {
		cfg.AggregateTimeout = 120 * time.Second
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/images/generations", h.withAuth(h.images))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.mux.HandleFunc("/v1/embeddings", h.withAuth(h.notFound))
	h.mux.HandleFunc("/", h.withAuth(h.notFound))
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != h.cfg.APIKey {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	writeOpenAIError(w, http.StatusNotFound, "not_found", "endpoint not found")
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": h.cfg.Pool.List(),
	})
}

// staticModels 静态回退模型表（SPEC §1.5 4320 的 11 模型）。
var staticModels = []map[string]any{
	{"id": "default", "object": "model", "created": 1753600000, "owned_by": "qclaw"},
	{"id": "pool-deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "qclaw"},
	{"id": "pool-deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "qclaw"},
	{"id": "pool-glm-5.2", "object": "model", "created": 1753600000, "owned_by": "qclaw"},
	{"id": "pool-glm-5.2-night", "object": "model", "created": 1753600000, "owned_by": "qclaw"},
	{"id": "pool-glm-5.1", "object": "model", "created": 1753600000, "owned_by": "qclaw"},
	{"id": "pool-hy3-preview", "object": "model", "created": 1753600000, "owned_by": "qclaw"},
	{"id": "pool-kimi-k2.7-code-highspeed", "object": "model", "created": 1753600000, "owned_by": "qclaw"},
	{"id": "pool-kimi-k2.6", "object": "model", "created": 1753600000, "owned_by": "qclaw"},
	{"id": "pool-minimax-m3", "object": "model", "created": 1753600000, "owned_by": "qclaw"},
	{"id": "pool-minimax-m2.7", "object": "model", "created": 1753600000, "owned_by": "qclaw"},
}

// modelCache 动态模型缓存。
type modelCache struct {
	sync.RWMutex
	ids      []string
	fetched  time.Time
	lastFail time.Time
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：优先 4320 动态（缓存 1h），失败回退静态表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(r.Context()),
	})
}

// modelList 动态获取模型列表并包装成 OpenAI 格式。
func (h *Handler) modelList(ctx context.Context) []map[string]any {
	if ids := h.fetchModelIDs(ctx); len(ids) > 0 {
		out := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			out = append(out, map[string]any{
				"id":       id,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "qclaw",
			})
		}
		return out
	}
	return staticModels
}

// fetchModelIDs 从池中任一健康账号调 4320 拉模型列表，缓存 1h；失败负缓存 5min。
func (h *Handler) fetchModelIDs(ctx context.Context) []string {
	h.modelCache.RLock()
	if len(h.modelCache.ids) > 0 && time.Since(h.modelCache.fetched) < dynamicModelsTTL {
		out := h.modelCache.ids
		h.modelCache.RUnlock()
		return out
	}
	if !h.modelCache.lastFail.IsZero() && time.Since(h.modelCache.lastFail) < modelsFetchFailCooldown {
		h.modelCache.RUnlock()
		return nil
	}
	h.modelCache.RUnlock()

	acct := h.cfg.Pool.Pick()
	if acct == nil {
		return nil
	}
	ms, err := h.cfg.JPRX.GetModelStatus(ctx, acct)
	if err != nil || len(ms.ModelStatusList) == 0 {
		h.modelCache.Lock()
		h.modelCache.lastFail = time.Now()
		h.modelCache.Unlock()
		return nil
	}
	ids := make([]string, 0, len(ms.ModelStatusList))
	for _, m := range ms.ModelStatusList {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	h.modelCache.Lock()
	h.modelCache.ids = ids
	h.modelCache.fetched = time.Now()
	h.modelCache.lastFail = time.Time{}
	h.modelCache.Unlock()
	return ids
}

// chatCompletions 对话主流程：鉴权 → pool 挑号 → 惰性 refresh → aizone → SSE/聚合。
func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UserID] = true

		// 本地 60rpm 令牌桶：超限换号（不触发冷却，只是跳过）
		if !h.cfg.Pool.ReserveToken(acct.UserID) {
			lastErr = errors.New("rpm limit exceeded locally")
			continue
		}

		// token 临近过期 → 惰性 4058 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.refreshAccount(r.Context(), acct); err != nil {
				lastErr = err
				continue
			}
		}

		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(r.Context(), acct, body)
		if terr != nil {
			// 客户端断开（ctx 已取消）：不算账号错误，不换号重试（P1-4）
			if errors.Is(terr, context.Canceled) || r.Context().Err() != nil {
				return
			}
			lastErr = terr
			h.cfg.Pool.NoteError(acct.UserID)
			continue
		}
		if status >= 400 {
			kind := upstream.Classify(status, string(respBody))
			switch kind {
			case upstream.ErrHardCredit:
				h.cfg.Pool.Cooldown(acct.UserID, pool.CoolCredit, h.cfg.HardCooldown, "余额不足/未激活")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			case upstream.ErrSoftRate:
				h.cfg.Pool.Cooldown(acct.UserID, pool.CoolRate, h.cfg.SoftCooldown, "429 rate limit")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			case upstream.ErrSessionDead:
				h.cfg.Pool.Disable(acct.UserID, "session dead, relogin required")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			default:
				h.cfg.Pool.NoteError(acct.UserID)
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			}
		}
		defer rc.Close()
		h.cfg.Pool.NoteSuccess(acct.UserID)
		if peek.Stream {
			_ = upstream.Stream(w, rc)
			return
		}
		// 非流式聚合：加总超时，上游挂死时按时返回错误（P1-5）
		aggCtx, cancel := context.WithTimeout(r.Context(), h.cfg.AggregateTimeout)
		defer cancel()
		resp, err := upstream.AggregateCtx(aggCtx, rc)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// refreshAccount 对账号做惰性 4058 refresh；失败返回错误（调用方冷却/换号）。
func (h *Handler) refreshAccount(ctx context.Context, acct *auth.Auth) error {
	if err := h.cfg.JPRX.RefreshChannelToken(ctx, acct); err != nil {
		var je *jprx.JPRXError
		if errors.As(err, &je) && je.IsLoginExpired() {
			h.cfg.Pool.Disable(acct.UserID, "refresh: 21004 session expired")
		} else {
			h.cfg.Pool.Cooldown(acct.UserID, pool.CoolErr, h.cfg.ErrCooldown, "refresh: "+err.Error())
		}
		return err
	}
	_ = acct.SaveAtomic()
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
