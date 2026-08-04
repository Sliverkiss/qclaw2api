package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"qclaw2api/internal/auth"
	"qclaw2api/internal/pool"
	"qclaw2api/internal/upstream"
)

const testAPIKey = "test-key"

func mkAuth(uid string) *auth.Auth {
	return &auth.Auth{
		JWTToken:     "jwt-" + uid,
		ChannelToken: "ct-" + uid,
		SKAPIKey:     "sk-test-" + uid,
		UserID:       uid,
		Nickname:     "用户" + uid,
		GUID:         "qclawmp_" + uid,
		ExpiresAt:    time.Now().Add(30 * 24 * time.Hour).Unix(),
	}
}

// fakeAizone 假 aizone 上游（返回 SSE 或 JSON）。
type fakeAizone struct {
	mu      sync.Mutex
	status  int
	body    string
	lastUA  string
	lastKey string
}

func (f *fakeAizone) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUA = r.Header.Get("User-Agent")
	f.lastKey = r.Header.Get("Authorization")
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(f.status)
	_, _ = w.Write([]byte(f.body))
}

// writeModelsFile 写测试 models.json（OpenAI /v1/models 形状）。
func writeModelsFile(t *testing.T, ids ...string) string {
	t.Helper()
	dir := t.TempDir()
	fp := filepath.Join(dir, "models.json")
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model", "created": 1753600000, "owned_by": "qclaw"})
	}
	raw, _ := json.Marshal(map[string]any{"object": "list", "data": data})
	if err := os.WriteFile(fp, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return fp
}

// newTestHandler 组装 handler：pool + aizone 假上游 + models 文件。
// modelsFile 为空则用默认（无文件 → 回退 staticModels）。
func newTestHandler(t *testing.T, aiz *fakeAizone, modelsFile string) (*Handler, *pool.Pool) {
	t.Helper()
	aizSrv := httptest.NewServer(http.HandlerFunc(aiz.handler))
	t.Cleanup(aizSrv.Close)

	p := pool.New("", pool.Config{RPM: 60, ErrThreshold: 5, ErrCooldown: 10 * time.Minute})
	p.Add(mkAuth("1"))
	p.Add(mkAuth("2"))

	up := upstream.New()
	up.SetChatURL(aizSrv.URL + "/aizone/v1/chat/completions")

	return NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		ModelsFile:   modelsFile,
		APIKey:       testAPIKey,
		MaxRotate:    3,
		SoftCooldown: 60 * time.Second,
		ErrThreshold: 5,
		ErrCooldown:  10 * time.Minute,
	}), p
}

// TestAuth 校验鉴权：缺 key → 401，错 key → 401，对 key → 200。
func TestAuth(t *testing.T) {
	body := `data: {"id":"x","choices":[{"delta":{"content":"hi"}}]}` + "\n\ndata: [DONE]\n\n"
	aiz := &fakeAizone{status: 200, body: body}
	h, _ := newTestHandler(t, aiz, writeModelsFile(t, "default"))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, _ := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing key: status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong key: status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":false}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("correct key: status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestChatAggregate 校验非流式聚合。
func TestChatAggregate(t *testing.T) {
	body := `data: {"id":"msg-1","model":"m","choices":[{"delta":{"content":"你好"}}]}` + "\n\ndata: [DONE]\n\n"
	aiz := &fakeAizone{status: 200, body: body}
	h, _ := newTestHandler(t, aiz, writeModelsFile(t, "default"))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":false}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["object"] != "chat.completion" {
		t.Errorf("object = %v", out["object"])
	}
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content = %v", msg["content"])
	}
}

// TestChatStream 校验流式透传 + UA + Bearer。
func TestChatStream(t *testing.T) {
	body := `data: {"id":"x","choices":[{"delta":{"content":"hi"}}]}` + "\n\ndata: [DONE]\n\n"
	aiz := &fakeAizone{status: 200, body: body}
	h, _ := newTestHandler(t, aiz, writeModelsFile(t, "default"))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	aiz.mu.Lock()
	defer aiz.mu.Unlock()
	if aiz.lastUA != "OpenAI/JS 6.39.1" {
		t.Errorf("UA = %q, want OpenAI/JS 6.39.1", aiz.lastUA)
	}
	if !strings.HasPrefix(aiz.lastKey, "Bearer sk-test-") {
		t.Errorf("Authorization = %q", aiz.lastKey)
	}
}

// TestChatRotateOnServerError 校验 5xx 换号后成功。
func TestChatRotateOnServerError(t *testing.T) {
	var calls int
	aizSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"ok","choices":[{"delta":{"content":"fine"}}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(aizSrv.Close)

	p := pool.New("", pool.Config{RPM: 60, ErrThreshold: 5, ErrCooldown: 10 * time.Minute})
	p.Add(mkAuth("1"))
	p.Add(mkAuth("2"))

	up := upstream.New()
	up.SetChatURL(aizSrv.URL + "/chat")

	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: testAPIKey, MaxRotate: 3})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":false}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if calls < 2 {
		t.Errorf("calls = %d, want >= 2 (rotate)", calls)
	}
}

// TestModels 校验 /v1/models 读 models.json 文件。
func TestModels(t *testing.T) {
	aiz := &fakeAizone{status: 200, body: ""}
	h, _ := newTestHandler(t, aiz, writeModelsFile(t, "default", "pool-deepseek-v4-flash"))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 2 {
		t.Errorf("models = %d, want 2", len(out.Data))
	}
}

// TestModelsStaticFallback 校验无 models.json 文件时回退静态表。
func TestModelsStaticFallback(t *testing.T) {
	aiz := &fakeAizone{status: 200, body: ""}
	h, _ := newTestHandler(t, aiz, "") // ModelsFile 空 → staticModels
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != len(staticModels) {
		t.Errorf("fallback models = %d, want %d", len(out.Data), len(staticModels))
	}
}

// TestModelsReloadFallbackCached 校验（P1-7）：models.json 损坏 → 回退 staticModels，
// 且 mtime 被记录为当前文件 ModTime，后续 list() 不再重复读盘重试（读放大消除）。
func TestModelsReloadFallbackCached(t *testing.T) {
	aiz := &fakeAizone{status: 200, body: ""}
	fp := writeModelsFile(t, "default")
	// 写坏内容（覆盖有效 JSON）
	if err := os.WriteFile(fp, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, _ := newTestHandler(t, aiz, fp)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// 首次触发 reload（启动时已加载成功，此处改文件 mtime 触发 reload 失败 → 回退）
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Data) != len(staticModels) {
		t.Fatalf("broken file should fallback to static, got %d models", len(out.Data))
	}

	// 再次请求应走缓存（mtime 已锁），仍回退 static 且不因读盘失败反复重试
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var out2 struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&out2)
	if len(out2.Data) != len(staticModels) {
		t.Errorf("second request = %d models, want static fallback", len(out2.Data))
	}
	// mtime 已锁定为损坏文件当前 ModTime（非 zero），避免每次 stat 不一致
	h.modelStore.mu.RLock()
	locked := !h.modelStore.mtime.IsZero()
	h.modelStore.mu.RUnlock()
	if !locked {
		t.Errorf("mtime not recorded after fallback (P1-7), read amplification persists")
	}
}

// TestStatus 校验 /status 返回账号列表。
func TestStatus(t *testing.T) {
	aiz := &fakeAizone{status: 200, body: ""}
	h, _ := newTestHandler(t, aiz, writeModelsFile(t, "default"))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/status", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 2 {
		t.Errorf("accounts = %d, want 2", len(out.Accounts))
	}
}

// TestHealthz 校验免鉴权 healthz。
func TestHealthz(t *testing.T) {
	aiz := &fakeAizone{status: 200, body: ""}
	h, _ := newTestHandler(t, aiz, writeModelsFile(t, "default"))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d", resp.StatusCode)
	}
}

// TestEmbeddingsNotFound 校验 embeddings 404。
func TestEmbeddingsNotFound(t *testing.T) {
	aiz := &fakeAizone{status: 200, body: ""}
	h, _ := newTestHandler(t, aiz, writeModelsFile(t, "default"))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/embeddings", strings.NewReader(`{"input":"x"}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("embeddings status = %d, want 404", resp.StatusCode)
	}
}

// TestChatTransportErrorNotCounted 校验传输错误不累计 err_count（R2）：
// 上游对每个账号都连接失败/超时（transport error）→ 不 NoteError，账号 err_count 保持 0，
// 最终 503 且文案带 "model timeout"（模型问题而非账号问题）。
func TestChatTransportErrorNotCounted(t *testing.T) {
	// 关闭的 httptest server：连接立即被拒 → transport error（而非 4xx/5xx）。
	aizSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := aizSrv.URL
	aizSrv.Close()

	p := pool.New("", pool.Config{RPM: 60, ErrThreshold: 5, ErrCooldown: 10 * time.Minute})
	p.Add(mkAuth("1"))
	p.Add(mkAuth("2"))

	up := upstream.New()
	up.SetChatURL(url + "/chat")

	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: testAPIKey, MaxRotate: 3})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":false}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	msg, _ := out["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "model timeout") {
		t.Errorf("503 message should mention model timeout (R2), got %q", msg)
	}

	// R2：传输错误不累计 err_count（账号未因模型超时受罚）
	for _, uid := range []string{"1", "2"} {
		st, ok := p.Status(uid)
		if !ok {
			t.Fatalf("status for %s missing", uid)
		}
		if st.ErrCount != 0 {
			t.Errorf("uid=%s ErrCount = %d, want 0 (transport error must not penalize account)", uid, st.ErrCount)
		}
		if st.Cooling || st.Disabled {
			t.Errorf("uid=%s should neither be cooled nor disabled: %+v", uid, st)
		}
	}
}

// TestChatClientDisconnectNotCounted 校验客户端断开（ctx 取消）不计账号错误（P1-4）。
// 直接构造已取消 ctx 的请求调用 chatCompletions：ChatStream 用该 ctx 发请求，
// chatHTTP.Do 立即返回 context.Canceled → handler 应直接 return，不 NoteError、不冷却账号。
func TestChatClientDisconnectNotCounted(t *testing.T) {
	aiz := &fakeAizone{status: 200, body: ""}
	h, p := newTestHandler(t, aiz, writeModelsFile(t, "default"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 请求 ctx 已取消（模拟客户端已断开）
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)

	rec := httptest.NewRecorder()
	h.chatCompletions(rec, req)

	// 断言 NoteError 未被调用：errCount 保持 0、未冷却
	st, _ := p.Status("1")
	if st.ErrCount != 0 {
		t.Errorf("ErrCount = %d, want 0 (client disconnect must not be counted)", st.ErrCount)
	}
	if st.Cooling {
		t.Errorf("account should not be cooled after client disconnect: %+v", st)
	}
	if st.Disabled {
		t.Errorf("account should not be disabled after client disconnect: %+v", st)
	}
}
