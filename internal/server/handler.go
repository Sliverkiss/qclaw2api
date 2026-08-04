// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + jprx/aizone 转发。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"qclaw2api/internal/pool"
	"qclaw2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	ModelsFile   string        // models.json 路径；空 = 用 staticModels
	APIKey       string        // 空 = 不鉴权
	MaxRotate    int           // 单请求最多换号次数，默认 3
	SoftCooldown time.Duration // 429/rpm 冷却，默认 60s
	ErrThreshold int           // 连续其他错误冷却阈值
	ErrCooldown  time.Duration // 错误冷却时长
	// AggregateTimeout 非流式聚合总超时（P1-5），默认 120s；上游挂死时按时返回错误。
	AggregateTimeout time.Duration
}

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	modelStore *modelsStore
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
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
	if cfg.AggregateTimeout <= 0 {
		cfg.AggregateTimeout = 120 * time.Second
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), modelStore: newModelsStore(cfg.ModelsFile)}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
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

// models 返回模型列表：读 models.json 文件（mtime 重载），失败回退静态表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelStore.list(),
	})
}

// chatCompletions 对话主流程：鉴权 → pool 挑号 → aizone → SSE/聚合。
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
	// R1：上游不返回 usage，网关按请求 messages 字符数估算 prompt_tokens。
	promptTokens := upstream.PromptTokensFromBody(body)

	tried := map[string]bool{}
	var lastErr error
	// R2：attempted 统计实际尝试过的账号；allTransportErr 记录是否全部为传输错误。
	// 无任何可试账号（attempted==0）或出现过 HTTP 响应 → 非模型超时。
	attempted := 0
	allTransportErr := true
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break // 没有更多可试账号
		}
		tried[acct.UserID] = true
		attempted++

		// 本地 60rpm 令牌桶：超限换号（不触发冷却，只是跳过）
		if !h.cfg.Pool.ReserveToken(acct.UserID) {
			allTransportErr = false
			lastErr = errors.New("rpm limit exceeded locally")
			continue
		}

		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(r.Context(), acct, body)
		if terr != nil {
			// 客户端断开（ctx 已取消）：不算账号错误，不换号重试（P1-4）
			if errors.Is(terr, context.Canceled) || r.Context().Err() != nil {
				return
			}
			// R2：传输错误（上游挂起/超时/断连）是模型问题不是账号问题——
			// 不累计 err_count，只换号重试（continue）。
			lastErr = terr
			continue
		}
		allTransportErr = false // 已拿到上游 HTTP 响应（含 4xx/5xx），本次失败不是传输错误
		if status >= 400 {
			kind := upstream.Classify(status, string(respBody))
			switch kind {
			case upstream.ErrHardCredit:
				// 积分不足 → 冷却至次日 0 点（R3），keepalive 每日探测恢复
				// 打印完整上游 body 便于校准 balanceMarkers 关键词（SPEC R1）
				log.Printf("chat uid=%s: hard_credit %d body=%s",
					acct.UserID, status, truncate(string(respBody), 200))
				h.cfg.Pool.CooldownCredit(acct.UserID, "积分不足，keepalive 探测恢复")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			case upstream.ErrInactive:
				// api_key_inactive 账号未激活，非积分问题 → CoolErr 10m
				h.cfg.Pool.Cooldown(acct.UserID, pool.CoolErr, h.cfg.ErrCooldown, "api_key_inactive 账号未激活")
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
			case upstream.ErrServer:
				h.cfg.Pool.NoteError(acct.UserID)
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			default:
				// ErrClient：其他 4xx，记录完整 body 便于校准关键词（SPEC R1）
				h.cfg.Pool.NoteError(acct.UserID)
				log.Printf("chat uid=%s: client error %d body=%s",
					acct.UserID, status, truncate(string(respBody), 200))
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			}
		}
		defer rc.Close()
		h.cfg.Pool.NoteSuccess(acct.UserID)
		if peek.Stream {
			_ = upstream.Stream(w, rc, promptTokens)
			return
		}
		// 非流式聚合：加总超时，上游挂死时按时返回错误（P1-5）
		aggCtx, cancel := context.WithTimeout(r.Context(), h.cfg.AggregateTimeout)
		defer cancel()
		resp, err := upstream.AggregateCtx(aggCtx, rc, promptTokens)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	// R2：若同一请求实际尝试过账号且全部因传输错误失败 → 503 模型不可用，
	// 且不累计任何账号 err_count（模型问题不是账号问题）。
	if attempted > 0 && allTransportErr && lastErr != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			"all accounts unavailable (model timeout): "+lastErr.Error())
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
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

// truncate 截断长文本用于错误信息/日志。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
