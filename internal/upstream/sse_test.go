package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseSample 是 SPEC §1.4 的 SSE 样本：流式 chunk 含 reasoning_content + content。
const sseSample = `data: {"id":"msg-123","object":"chat.completion.chunk","created":1783000000,"model":"pool-deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant"}}]}

data: {"choices":[{"delta":{"reasoning_content":"We"}}]}

data: {"choices":[{"delta":{"reasoning_content":" think"}}]}

data: {"choices":[{"delta":{"content":"你好！"}}]}

data: {"choices":[{"finish_reason":"stop"}]}

data: [DONE]

`

// TestAggregateBasic 校验流式聚合：content/reasoning_content 拼接、无 usage。
func TestAggregateBasic(t *testing.T) {
	resp, err := Aggregate(strings.NewReader(sseSample))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if resp["id"] != "msg-123" {
		t.Errorf("id = %v", resp["id"])
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object = %v", resp["object"])
	}
	if resp["model"] != "pool-deepseek-v4-flash" {
		t.Errorf("model = %v", resp["model"])
	}
	choices := resp["choices"].([]any)
	c0 := choices[0].(map[string]any)
	msg := c0["message"].(map[string]any)
	if msg["content"] != "你好！" {
		t.Errorf("content = %v", msg["content"])
	}
	if msg["reasoning_content"] != "We think" {
		t.Errorf("reasoning_content = %v", msg["reasoning_content"])
	}
	if c0["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v", c0["finish_reason"])
	}
	// SPEC §1.4：非流式无 usage 字段
	if _, ok := resp["usage"]; ok {
		t.Errorf("usage should NOT be present (SPEC §1.4)")
	}
}

// TestAggregateToolCalls 校验 tool_calls 按 index 合并。
func TestAggregateToolCalls(t *testing.T) {
	sse := `data: {"id":"msg-1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]}}]}

data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choices := resp["choices"].([]any)
	c0 := choices[0].(map[string]any)
	msg := c0["message"].(map[string]any)
	tcs, ok := asSlice(msg["tool_calls"])
	if !ok {
		t.Fatalf("tool_calls not slice: %T", msg["tool_calls"])
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_1" {
		t.Errorf("tool call id = %v", tc["id"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("function name = %v", fn["name"])
	}
	if fn["arguments"] != `{"city":"北京"}` {
		t.Errorf("arguments = %v", fn["arguments"])
	}
}

// TestAggregateEmpty 空流 → 默认 id + 空 content。
func TestAggregateEmpty(t *testing.T) {
	resp, err := Aggregate(strings.NewReader(""))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if resp["id"] == "" {
		t.Errorf("id should be generated")
	}
	choices := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "" {
		t.Errorf("content = %v", msg["content"])
	}
}

// TestAggregateNoDone 缺 [DONE] 时正常聚合（EOF 截断）。
func TestAggregateNoDone(t *testing.T) {
	sse := `data: {"id":"msg-x","model":"m","choices":[{"delta":{"content":"part"}}]}

`
	resp, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choices := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "part" {
		t.Errorf("content = %v", msg["content"])
	}
}

// TestStreamPassthrough 校验透传逐行 flush 且缺 [DONE] 补发。
func TestStreamPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &flusherRecorder{ResponseRecorder: rec}
	err := Stream(w, strings.NewReader(sseSample))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"reasoning_content":"We"`,
		`"content":"你好！"`,
		`data: [DONE]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
}

// TestStreamMissingDone 缺 [DONE] 时补发。
func TestStreamMissingDone(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &flusherRecorder{ResponseRecorder: rec}
	err := Stream(w, strings.NewReader(`data: {"id":"x"}`+"\n\n"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("missing appended [DONE]: %q", body)
	}
}

// flusherRecorder 包装 httptest.ResponseRecorder 实现 http.Flusher。
type flusherRecorder struct {
	*httptest.ResponseRecorder
}

func (f *flusherRecorder) Flush() {}

// TestClassify 校验错误分类映射（SPEC §5）。
func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{401, `{"error":"invalid_api_key"}`, ErrSessionDead},
		{403, `{"error":"api_key_inactive"}`, ErrInactive},
		// P1-8：api_key_inactive 与余额关键词并存时，inactive 优先。
		{403, `{"error":"api_key_inactive: 余额不足，请充值"}`, ErrInactive},
		{403, `{"error":"insufficient credit"}`, ErrHardCredit},
		// P1-3：中文余额/停用文案。
		{402, `{"error":"欠费，请充值"}`, ErrHardCredit},
		{402, `{"error":"余额为 0"}`, ErrHardCredit},
		{402, `{"error":"余额为0"}`, ErrHardCredit},
		{402, `{"error":"余额为零"}`, ErrHardCredit},
		{402, `{"error":"账号已停用"}`, ErrHardCredit},
		{429, `{"error":"rate limited"}`, ErrSoftRate},
		{500, `internal error`, ErrServer},
		{400, `invalid request`, ErrClient},
		{200, `{"code":9002,"message":"该功能暂不可用"}`, ErrNone},
	}
	for i, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("case %d: Classify(%d,%q) = %v, want %v", i, c.status, c.body, got, c.want)
		}
	}
}

// TestJSONMarshalAggregate 校验聚合结果可 JSON 序列化（server 响应用）。
func TestJSONMarshalAggregate(t *testing.T) {
	resp, err := Aggregate(strings.NewReader(sseSample))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if _, err := json.Marshal(resp); err != nil {
		t.Errorf("marshal: %v", err)
	}
}

// TestAggregateNonStreamJSON 校验 stream:false 纯 JSON 响应（无 data: 前缀）直接透传。
// 复现真实上游形态（SPEC §1.4 实测非流式响应）——无 usage、message 含 content/reasoning_content。
func TestAggregateNonStreamJSON(t *testing.T) {
	raw := `{"id":"msg-abc","object":"chat.completion","created":1783000000,"model":"pool-deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"5+5等于10。","reasoning_content":"先算5+5"},"finish_reason":"stop"}]}`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if resp["id"] != "msg-abc" {
		t.Errorf("id = %v", resp["id"])
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object = %v", resp["object"])
	}
	choices, ok := asSlice(resp["choices"])
	if !ok || len(choices) != 1 {
		t.Fatalf("choices = %v", resp["choices"])
	}
	c0 := choices[0].(map[string]any)
	msg := c0["message"].(map[string]any)
	if msg["content"] != "5+5等于10。" {
		t.Errorf("content = %v", msg["content"])
	}
	if msg["reasoning_content"] != "先算5+5" {
		t.Errorf("reasoning_content = %v", msg["reasoning_content"])
	}
	if c0["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v", c0["finish_reason"])
	}
	// SPEC §1.4：非流式无 usage 字段
	if _, ok := resp["usage"]; ok {
		t.Errorf("usage should NOT be present (SPEC §1.4)")
	}
}

// TestAggregateNonStreamMissingFields 校验纯 JSON 缺 role/finish_reason/object 时归一化补齐。
func TestAggregateNonStreamMissingFields(t *testing.T) {
	raw := `{"choices":[{"message":{"content":"hi"}}]}`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choices, _ := asSlice(resp["choices"])
	c0 := choices[0].(map[string]any)
	msg := c0["message"].(map[string]any)
	if msg["role"] != "assistant" {
		t.Errorf("role = %v (want assistant)", msg["role"])
	}
	if c0["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v (want stop)", c0["finish_reason"])
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object = %v (want chat.completion)", resp["object"])
	}
}

// TestAggregateCtxTimeout 校验上游挂死时按 ctx 超时返回错误（P1-5）。
// io.Pipe 永不写入/关闭 → Aggregate 阻塞读；200ms ctx → 按时返回 context.DeadlineExceeded。
func TestAggregateCtxTimeout(t *testing.T) {
	pr, _ := io.Pipe() // 永不写、永不关 → 读端永久阻塞
	defer pr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := AggregateCtx(ctx, pr)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("AggregateCtx took %v, want ~200ms", elapsed)
	}
}
