package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"qclaw2api/internal/auth"
	"qclaw2api/internal/jprx"
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

// fakeJPRX 假 jprx 网关（模型列表）。
type fakeJPRX struct {
	mu     sync.Mutex
	models []string
}

func (f *fakeJPRX) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := []map[string]any{}
	for _, m := range f.models {
		items = append(items, map[string]any{"id": m, "name": m})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ret": 0,
		"data": map[string]any{
			"resp": map[string]any{
				"common": map[string]any{"code": 0},
				"data":   map[string]any{"model_status_list": items},
			},
		},
	})
}

// newTestHandler 组装 handler：pool + aizone 假上游 + jprx 假网关。
func newTestHandler(t *testing.T, aiz *fakeAizone, j *fakeJPRX) (*Handler, *pool.Pool) {
	t.Helper()
	aizSrv := httptest.NewServer(http.HandlerFunc(aiz.handler))
	t.Cleanup(aizSrv.Close)
	jSrv := httptest.NewServer(http.HandlerFunc(j.handler))
	t.Cleanup(jSrv.Close)

	p := pool.New("", pool.Config{RPM: 60, ErrThreshold: 5, ErrCooldown: 10 * time.Minute})
	p.Add(mkAuth("1"))
	p.SetCredits("1", 100)
	p.Add(mkAuth("2"))
	p.SetCredits("2", 300)

	up := upstream.New()
	up.SetChatURL(aizSrv.URL + "/aizone/v1/chat/completions")

	jc := jprx.New()
	jc.SetBase(jSrv.URL)

	return NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		JPRX:         jc,
		APIKey:       testAPIKey,
		MaxRotate:    3,
		HardCooldown: 12 * time.Hour,
		SoftCooldown: 60 * time.Second,
		ErrThreshold: 5,
		ErrCooldown:  10 * time.Minute,
		RefreshSkew:  7 * 24 * time.Hour,
	}), p
}

// TestAuth 校验鉴权：缺 key → 401，错 key → 401，对 key → 200。
func TestAuth(t *testing.T) {
	body := `data: {"id":"x","choices":[{"delta":{"content":"hi"}}]}` + "\n\ndata: [DONE]\n\n"
	aiz := &fakeAizone{status: 200, body: body}
	h, _ := newTestHandler(t, aiz, &fakeJPRX{models: []string{"default"}})
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
	h, _ := newTestHandler(t, aiz, &fakeJPRX{models: []string{"default"}})
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
	h, _ := newTestHandler(t, aiz, &fakeJPRX{models: []string{"default"}})
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
	p.SetCredits("1", 100)
	p.Add(mkAuth("2"))
	p.SetCredits("2", 300)

	up := upstream.New()
	up.SetChatURL(aizSrv.URL + "/chat")
	jc := jprx.New()

	h := NewHandler(Config{Pool: p, Upstream: up, JPRX: jc, APIKey: testAPIKey, MaxRotate: 3})
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

// TestModels 校验 /v1/models 动态列表（经 4320 假网关）。
func TestModels(t *testing.T) {
	aiz := &fakeAizone{status: 200, body: ""}
	h, _ := newTestHandler(t, aiz, &fakeJPRX{models: []string{"default", "pool-deepseek-v4-flash"}})
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

// TestModelsStaticFallback 校验 4320 失败/无账号时回退静态表。
func TestModelsStaticFallback(t *testing.T) {
	aiz := &fakeAizone{status: 200, body: ""}
	j := &fakeJPRX{models: []string{}}
	h, _ := newTestHandler(t, aiz, j)
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

// TestStatus 校验 /status 返回账号列表。
func TestStatus(t *testing.T) {
	aiz := &fakeAizone{status: 200, body: ""}
	h, _ := newTestHandler(t, aiz, &fakeJPRX{models: []string{"default"}})
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
	h, _ := newTestHandler(t, aiz, &fakeJPRX{models: []string{"default"}})
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
	h, _ := newTestHandler(t, aiz, &fakeJPRX{models: []string{"default"}})
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

// TestChatClientDisconnectNotCounted 校验客户端断开（ctx 取消）不计账号错误（P1-4）。
// 直接构造已取消 ctx 的请求调用 chatCompletions：ChatStream 用该 ctx 发请求，
// chatHTTP.Do 立即返回 context.Canceled → handler 应直接 return，不 NoteError、不冷却账号。
func TestChatClientDisconnectNotCounted(t *testing.T) {
	aiz := &fakeAizone{status: 200, body: ""}
	h, p := newTestHandler(t, aiz, &fakeJPRX{models: []string{"default"}})

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
