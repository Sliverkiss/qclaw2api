package jprx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"qclaw2api/internal/auth"
)

// newTestAuth 构造带 FilePath 的测试账号。
func newTestAuth(t *testing.T) (*auth.Auth, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "qclaw-123456789.json")
	a := &auth.Auth{
		JWTToken:     "jwt-old",
		ChannelToken: "ct-test",
		SKAPIKey:     "sk-test-1234",
		UserID:       "123456789",
		GUID:         "qclawmp_test-guid",
		ExpiresAt:    1783000000,
		FilePath:     path,
	}
	return a, path
}

// fakeJPRX 构造一个可编程的 jprx 假网关。
type fakeJPRX struct {
	t        *testing.T
	paths    map[string]func(w http.ResponseWriter, r *http.Request)
	newToken string
	lastPath string
	lastBody map[string]any
}

func (f *fakeJPRX) handler(w http.ResponseWriter, r *http.Request) {
	f.lastPath = r.URL.Path
	_ = json.NewDecoder(r.Body).Decode(&f.lastBody)
	if f.newToken != "" {
		w.Header().Set("X-New-Token", f.newToken)
	}
	fn := f.paths[r.URL.Path]
	if fn == nil {
		http.Error(w, "unexpected path", http.StatusNotFound)
		return
	}
	fn(w, r)
}

// newTestClient 起假网关并返回 jprx Client（base 指向假网关）。
func newTestClient(t *testing.T, f *fakeJPRX) (*Client, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	c := New()
	c.base = srv.URL
	return c, srv.URL
}

func respOK(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// TestEnvelopeForms 校验三种信封形态解析（数据集 / 4055 / 扁平）。
func TestEnvelopeForms(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"data set form", `{"ret":0,"data":{"resp":{"common":{"code":0,"message":"ok"},"data":{"state":"abc"}}}}`, false},
		{"4055 form", `{"common":{"code":0},"data":{"key":"sk-test"}}`, false},
		{"flat form", `{"code":0,"data":{"x":1},"message":"ok"}`, false},
		{"business error", `{"ret":0,"data":{"resp":{"common":{"code":21004,"message":"登录已过期"},"data":{}}}}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, payload, msg, err := parseEnvelope([]byte(c.raw))
			if c.wantErr {
				if err == nil && code == 0 {
					t.Errorf("expected business error, got code=%d", code)
				}
				if err == nil && msg == "" {
					t.Errorf("expected message captured")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEnvelope: %v", err)
			}
			if code != 0 {
				t.Errorf("code = %d, want 0", code)
			}
			if len(payload) == 0 {
				t.Errorf("payload empty")
			}
		})
	}
}

// Test4050WxLoginState 校验 4050 state 提取。
func Test4050WxLoginState(t *testing.T) {
	f := &fakeJPRX{
		t: t,
		paths: map[string]func(w http.ResponseWriter, r *http.Request){
			"/data/4050/forward": func(w http.ResponseWriter, r *http.Request) {
				respOK(w, `{"ret":0,"data":{"resp":{"common":{"code":0,"message":"ok"},"data":{"state":"state_abc"}}}}`)
			},
		},
	}
	c, _ := newTestClient(t, f)
	state, err := c.WxLoginState(context.Background(), "qclawmp_test")
	if err != nil {
		t.Fatalf("WxLoginState: %v", err)
	}
	if state != "state_abc" {
		t.Errorf("state = %q, want state_abc", state)
	}
	if f.lastPath != "/data/4050/forward" {
		t.Errorf("path = %q", f.lastPath)
	}
	if f.lastBody["guid"] != "qclawmp_test" {
		t.Errorf("guid = %v", f.lastBody["guid"])
	}
}

// Test4026WxLogin 校验 4026 登录结果与 is_new_user。
func Test4026WxLogin(t *testing.T) {
	f := &fakeJPRX{
		t: t,
		paths: map[string]func(w http.ResponseWriter, r *http.Request){
			"/data/4026/forward": func(w http.ResponseWriter, r *http.Request) {
				respOK(w, `{"ret":0,"data":{"resp":{"common":{"code":0},"data":{
					"token":"jwt-new","expires_in":2592000,
					"openclaw_channel_token":"ct-new",
					"user_info":{"user_id":123456789,"nickname":"测试"},
					"is_new_user":false}}}}`)
			},
		},
	}
	c, _ := newTestClient(t, f)
	res, err := c.WxLogin(context.Background(), "guid", "code123", "state")
	if err != nil {
		t.Fatalf("WxLogin: %v", err)
	}
	if res.JWTToken != "jwt-new" || res.UserID != "123456789" || res.IsNewUser {
		t.Errorf("unexpected login result: %+v", res)
	}
	if res.ChannelToken != "ct-new" {
		t.Errorf("ChannelToken = %q", res.ChannelToken)
	}
}

// Test4026NewUser 校验 is_new_user 透传。
func Test4026NewUser(t *testing.T) {
	f := &fakeJPRX{
		t: t,
		paths: map[string]func(w http.ResponseWriter, r *http.Request){
			"/data/4026/forward": func(w http.ResponseWriter, r *http.Request) {
				respOK(w, `{"ret":0,"data":{"resp":{"common":{"code":0},"data":{
					"token":"jwt-new","is_new_user":true}}}}`)
			},
		},
	}
	c, _ := newTestClient(t, f)
	res, err := c.WxLogin(context.Background(), "guid", "code", "state")
	if err != nil {
		t.Fatalf("WxLogin: %v", err)
	}
	if !res.IsNewUser {
		t.Errorf("IsNewUser = false, want true")
	}
}

// Test4055APIV1Path 校验 4055 走 /api/v1/4055 且 body 固定。
func Test4055APIV1Path(t *testing.T) {
	a, _ := newTestAuth(t)
	f := &fakeJPRX{
		t: t,
		paths: map[string]func(w http.ResponseWriter, r *http.Request){
			"/api/v1/4055": func(w http.ResponseWriter, r *http.Request) {
				respOK(w, `{"common":{"code":0},"data":{"key":"sk-test-4055","masked_key":"sk-t****4055","key_hash":"h","created_at":"2026"}}`)
			},
		},
	}
	c, _ := newTestClient(t, f)
	key, err := c.GetAPIKey(context.Background(), a)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if key.Key != "sk-test-4055" {
		t.Errorf("Key = %q", key.Key)
	}
	if f.lastPath != "/api/v1/4055" {
		t.Errorf("path = %q, want /api/v1/4055", f.lastPath)
	}
	if f.lastBody["web_version"] != "1.4.0" || f.lastBody["web_env"] != "release" {
		t.Errorf("body = %v", f.lastBody)
	}
	// 登录态头
	if a.JWTToken == "" {
		t.Errorf("auth not attached")
	}
}

// TestNewTokenCapture 校验 X-New-Token 捕获并原子落盘。
func TestNewTokenCapture(t *testing.T) {
	a, path := newTestAuth(t)
	f := &fakeJPRX{
		t:        t,
		newToken: "jwt-refreshed",
		paths: map[string]func(w http.ResponseWriter, r *http.Request){
			"/data/4058/forward": func(w http.ResponseWriter, r *http.Request) {
				respOK(w, `{"ret":0,"data":{"resp":{"common":{"code":0},"data":{"openclaw_channel_token":"ct-same"}}}}`)
			},
		},
	}
	c, _ := newTestClient(t, f)
	if err := c.RefreshChannelToken(context.Background(), a); err != nil {
		t.Fatalf("RefreshChannelToken: %v", err)
	}
	if a.JWTToken != "jwt-refreshed" {
		t.Errorf("JWTToken = %q, want jwt-refreshed", a.JWTToken)
	}
	// 落盘验证
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	got, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if got.JWTToken != "jwt-refreshed" {
		t.Errorf("disk JWTToken = %q", got.JWTToken)
	}
}

// TestNewTokenSameValue 校验同值 X-New-Token 不重复落盘。
func TestNewTokenSameValue(t *testing.T) {
	a, path := newTestAuth(t)
	f := &fakeJPRX{
		t:        t,
		newToken: "jwt-old", // 与当前相同
		paths: map[string]func(w http.ResponseWriter, r *http.Request){
			"/data/4058/forward": func(w http.ResponseWriter, r *http.Request) {
				respOK(w, `{"ret":0,"data":{"resp":{"common":{"code":0},"data":{"x":1}}}}`)
			},
		},
	}
	c, _ := newTestClient(t, f)
	if err := c.RefreshChannelToken(context.Background(), a); err != nil {
		t.Fatalf("RefreshChannelToken: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("same-value token should not rewrite file")
	}
}

// Test4110QBalance 校验 Q 点余额解析。
func Test4110QBalance(t *testing.T) {
	a, _ := newTestAuth(t)
	f := &fakeJPRX{
		t: t,
		paths: map[string]func(w http.ResponseWriter, r *http.Request){
			"/data/4110/forward": func(w http.ResponseWriter, r *http.Request) {
				respOK(w, `{"ret":0,"data":{"resp":{"common":{"code":0},"data":{
					"balance":2000,
					"balance_detail":{"activity_q":2000,"items":[
						{"label":"新用户注册赠送","total_amount":2000,"remain_amount":2000,"expire_time":"2026-10-31"}
					]}}}}}`)
			},
		},
	}
	c, _ := newTestClient(t, f)
	qb, err := c.GetQBalance(context.Background(), a)
	if err != nil {
		t.Fatalf("GetQBalance: %v", err)
	}
	if qb.Balance != 2000 {
		t.Errorf("Balance = %v", qb.Balance)
	}
	if len(qb.BalanceDetail.Items) != 1 || qb.BalanceDetail.Items[0].RemainAmount != 2000 {
		t.Errorf("items = %+v", qb.BalanceDetail.Items)
	}
}

// Test4075TodayTokens 校验今日额度解析。
func Test4075TodayTokens(t *testing.T) {
	a, _ := newTestAuth(t)
	f := &fakeJPRX{
		t: t,
		paths: map[string]func(w http.ResponseWriter, r *http.Request){
			"/data/4075/forward": func(w http.ResponseWriter, r *http.Request) {
				respOK(w, `{"ret":0,"data":{"resp":{"common":{"code":0},"data":{
					"daily_token_limit":40000000,"daily_token_used":0,"rpm_limit":60}}}}`)
			},
		},
	}
	c, _ := newTestClient(t, f)
	tt, err := c.GetTodayTokens(context.Background(), a)
	if err != nil {
		t.Fatalf("GetTodayTokens: %v", err)
	}
	if tt.RPMLimit != 60 || tt.DailyTokenLimit != 40000000 {
		t.Errorf("tt = %+v", tt)
	}
}

// Test4320ModelStatus 校验模型列表解析。
func Test4320ModelStatus(t *testing.T) {
	a, _ := newTestAuth(t)
	f := &fakeJPRX{
		t: t,
		paths: map[string]func(w http.ResponseWriter, r *http.Request){
			"/data/4320/forward": func(w http.ResponseWriter, r *http.Request) {
				respOK(w, `{"ret":0,"data":{"resp":{"common":{"code":0},"data":{
					"model_status_list":[{"id":"default","name":"Auto"},{"id":"pool-deepseek-v4-flash","name":"DeepSeek V4"}]}}}}`)
			},
		},
	}
	c, _ := newTestClient(t, f)
	ms, err := c.GetModelStatus(context.Background(), a)
	if err != nil {
		t.Fatalf("GetModelStatus: %v", err)
	}
	if len(ms.ModelStatusList) != 2 || ms.ModelStatusList[0].ID != "default" {
		t.Errorf("models = %+v", ms.ModelStatusList)
	}
}

// Test4299GenerateImage 校验生图状态解析。
func Test4299GenerateImage(t *testing.T) {
	a, _ := newTestAuth(t)
	f := &fakeJPRX{
		t: t,
		paths: map[string]func(w http.ResponseWriter, r *http.Request){
			"/data/4299/forward": func(w http.ResponseWriter, r *http.Request) {
				respOK(w, `{"ret":0,"data":{"resp":{"common":{"code":0},"data":{
					"status":"pending","request_id":"req-1"}}}}`)
			},
		},
	}
	c, _ := newTestClient(t, f)
	res, err := c.GenerateImage(context.Background(), a, "a cat")
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if res.Status != "pending" || res.RequestID != "req-1" {
		t.Errorf("res = %+v", res)
	}
	if f.lastBody["prompt"] != "a cat" {
		t.Errorf("prompt = %v", f.lastBody["prompt"])
	}
}

// Test21004LoginExpired 校验 21004 业务错误分类。
func Test21004LoginExpired(t *testing.T) {
	a, _ := newTestAuth(t)
	f := &fakeJPRX{
		t: t,
		paths: map[string]func(w http.ResponseWriter, r *http.Request){
			"/data/4058/forward": func(w http.ResponseWriter, r *http.Request) {
				respOK(w, `{"ret":0,"data":{"resp":{"common":{"code":21004,"message":"登录已过期"},"data":{}}}}`)
			},
		},
	}
	c, _ := newTestClient(t, f)
	err := c.RefreshChannelToken(context.Background(), a)
	if err == nil {
		t.Fatal("expected error")
	}
	je, ok := err.(*JPRXError)
	if !ok {
		t.Fatalf("error type = %T, want *JPRXError", err)
	}
	if !je.IsLoginExpired() {
		t.Errorf("IsLoginExpired = false")
	}
}

// TestHTTPError 校验 HTTP 5xx 返回错误。
func TestHTTPError(t *testing.T) {
	a, _ := newTestAuth(t)
	f := &fakeJPRX{
		t: t,
		paths: map[string]func(w http.ResponseWriter, r *http.Request){
			"/data/4058/forward": func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"error":"boom"}`, http.StatusBadGateway)
			},
		},
	}
	c, _ := newTestClient(t, f)
	if err := c.RefreshChannelToken(context.Background(), a); err == nil {
		t.Fatal("expected error on 502")
	}
}

// TestSignHeaders 校验签名头完整性与登录态头。
func TestSignHeaders(t *testing.T) {
	a, _ := newTestAuth(t)
	req := httptest.NewRequest(http.MethodPost, "http://x/", nil)
	body := []byte(`{"guid":"qclawmp_test-guid"}`)
	if err := signHeaders(req.Header, body, a); err != nil {
		t.Fatalf("signHeaders: %v", err)
	}
	if req.Header.Get("X-OpenClaw-Token") != "jwt-old" {
		t.Errorf("X-OpenClaw-Token = %q", req.Header.Get("X-OpenClaw-Token"))
	}
	if req.Header.Get("X-Guid") != "qclawmp_test-guid" {
		t.Errorf("X-Guid = %q", req.Header.Get("X-Guid"))
	}
	if req.Header.Get("X-Account-Id") != "123456789" {
		t.Errorf("X-Account-Id = %q", req.Header.Get("X-Account-Id"))
	}
	if req.Header.Get("X-Sign-Signature") == "" {
		t.Errorf("X-Sign-Signature missing")
	}
	if req.Header.Get("X-OpenClaw-ClientVersion") != "1.4.0" {
		t.Errorf("ClientVersion = %q", req.Header.Get("X-OpenClaw-ClientVersion"))
	}
}

// TestConcurrentSignAndCapture 校验 captureNewToken 持锁写 vs signHeaders/NeedsRefresh
// 无锁读侧并发无数据竞争（F4）。
func TestConcurrentSignAndCapture(t *testing.T) {
	a, _ := newTestAuth(t)
	body := []byte(`{"guid":"qclawmp_test-guid"}`)
	var wg sync.WaitGroup

	// 写侧：X-New-Token 每轮取不同值 → captureNewToken 实际改写 JWTToken
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			h := http.Header{}
			h.Set("X-New-Token", fmt.Sprintf("jwt-refresh-%d", i))
			_ = captureNewToken(h, a)
		}
	}()
	// 读侧：signHeaders + NeedsRefresh 无锁直读
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				h := http.Header{}
				if err := signHeaders(h, body, a); err != nil {
					t.Errorf("signHeaders: %v", err)
				}
				_ = a.NeedsRefresh(7 * 24 * time.Hour)
			}
		}()
	}
	wg.Wait()
}
