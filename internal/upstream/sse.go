// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI chat.completion 响应。
// 分片/半行由 bufio.Reader.ReadString 处理；遇到 "data: [DONE]" 结束。
// reasoning_content 拼入 message；tool_calls 按 index 合并（首片带 id/type/name）。
// 上游不返回 usage（实测），聚合完成后按本地近似估算补齐 OpenAI 标准 usage（R1）。
// promptTokens 由调用方从请求 body 估算（PromptTokensFromBody）。
// 兼容两种上游形态：纯 JSON（stream:false，无 data: 前缀）与 SSE 流。
func Aggregate(r io.Reader, promptTokens int) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	// stream:false 时上游返回纯 JSON（首字节 {），非 SSE —— 整体解析直接返回。
	if first, err := br.Peek(1); err == nil && first[0] == '{' {
		raw, err := io.ReadAll(br)
		if err != nil {
			return nil, err
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
		return attachUsage(normalizeNonStream(out), promptTokens, extractContent(out)), nil
	}
	var (
		id, model     string
		created       float64
		content       strings.Builder
		reasoning     strings.Builder
		role          = "assistant"
		finishReason  = "stop"
		gotAnyContent bool
		toolCalls     = map[int]map[string]any{}
		toolOrder     []int
	)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload != "[DONE]" {
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					if v, ok := chunk["id"].(string); ok && id == "" {
						id = v
					}
					if v, ok := chunk["model"].(string); ok && model == "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && created == 0 {
						created = v
					}
					if ch, ok := asSlice(chunk["choices"]); ok {
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							if fr, ok := c["finish_reason"].(string); ok && fr != "" {
								finishReason = fr
							}
							if delta, ok := c["delta"].(map[string]any); ok {
								if r2, ok := delta["role"].(string); ok && r2 != "" {
									role = r2
								}
								if txt, ok := delta["content"].(string); ok {
									content.WriteString(txt)
									gotAnyContent = true
								}
								if rc, ok := delta["reasoning_content"].(string); ok {
									reasoning.WriteString(rc)
								}
								if tcs, ok := asSlice(delta["tool_calls"]); ok {
									for _, tc := range tcs {
										call, ok := tc.(map[string]any)
										if !ok {
											continue
										}
										idx := 0
										if v, ok := call["index"].(float64); ok {
											idx = int(v)
										}
										merged, seen := toolCalls[idx]
										if !seen {
											merged = map[string]any{"index": idx}
											toolCalls[idx] = merged
											toolOrder = append(toolOrder, idx)
										}
										mergeToolCallDelta(merged, call)
									}
								}
							}
							// 兼容上游把完整消息放 message 里（非 delta）
							if msg, ok := c["message"].(map[string]any); ok && !gotAnyContent {
								if txt, ok := msg["content"].(string); ok {
									content.WriteString(txt)
								}
							}
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	// 上游不返回 usage（实测），这里按字符数近似估算补齐（R1）。
	return attachUsage(resp, promptTokens, content.String()), nil
}

// AggregateCtx 是 Aggregate 的 context 感知版本（P1-5）：上游 body 挂死时
// 按 ctx 超时返回错误，避免非流式请求永久阻塞。
// 注：底层 Aggregate 阻塞读时 goroutine 会滞留至上游结束——这正是挂死保护场景，
// 超时返回后调用方关闭连接即可回收资源。
func AggregateCtx(ctx context.Context, r io.Reader, promptTokens int) (map[string]any, error) {
	type res struct {
		resp map[string]any
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		resp, err := Aggregate(r, promptTokens)
		ch <- res{resp, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.resp, r.err
	}
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// asSlice 兼容 JSON 反序列化出的两种切片形态：[]any 与 []map[string]any。
// Go 泛型反序列化嵌套对象时，元素类型可能是 map[string]any 而非 any。
func asSlice(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case []map[string]any:
		out := make([]any, 0, len(t))
		for _, m := range t {
			out = append(out, m)
		}
		return out, true
	default:
		return nil, false
	}
}

// sortInts 升序排序（避免引 sort 包只为三行）。
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// normalizeNonStream 归一化纯 JSON 非流式响应（上游 stream:false 直接返回完整
// chat.completion JSON）。确保 OpenAI 兼容字段齐全：message.role 默认 assistant、
// finish_reason 默认 stop、reasoning_content 保留。
func normalizeNonStream(out map[string]any) map[string]any {
	choices, ok := asSlice(out["choices"])
	if !ok || len(choices) == 0 {
		return out
	}
	c, ok := choices[0].(map[string]any)
	if !ok {
		return out
	}
	if msg, ok := c["message"].(map[string]any); ok {
		if _, has := msg["role"]; !has {
			msg["role"] = "assistant"
		}
	}
	if _, has := c["finish_reason"]; !has {
		c["finish_reason"] = "stop"
	}
	if _, has := out["object"]; !has {
		out["object"] = "chat.completion"
	}
	return out
}

// Stream 透传上游 SSE 到 w（每行 flush），保证至少写一个 [DONE]。
// 上游不返回 usage（实测）→ 在 [DONE] 前注入一个带 usage 的 chunk
// （OpenAI 标准：流式最后一块带 usage、choices 为空数组）（R1）。
// promptTokens 由调用方从请求 body 估算（PromptTokensFromBody）。
// 调用方必须先设置过 status 200；本函数自设 SSE headers。
func Stream(w http.ResponseWriter, r io.Reader, promptTokens int) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model string
		created   float64
		content   strings.Builder
		sawDone   bool
		sawUsage  bool
	)
	writeLine := func(s string) error {
		if _, err := io.WriteString(w, s); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data: ") {
				payload := strings.TrimPrefix(trimmed, "data: ")
				if payload == "[DONE]" {
					sawDone = true
					// 在 [DONE] 前注入带 usage 的 chunk（上游从未自带 usage）。
					if !sawUsage {
						if werr := writeLine(usageChunk(id, model, created, promptTokens, content.String())); werr != nil {
							return werr
						}
					}
					if werr := writeLine(line); werr != nil {
						return werr
					}
					continue
				}
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					if v, ok := chunk["id"].(string); ok && v != "" {
						id = v
					}
					if v, ok := chunk["model"].(string); ok && v != "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && v != 0 {
						created = v
					}
					if ch, ok := asSlice(chunk["choices"]); ok {
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							if d, ok := c["delta"].(map[string]any); ok {
								if txt, ok := d["content"].(string); ok {
									content.WriteString(txt)
								}
							}
							// 兼容上游把完整消息放 message 里（非 delta）
							if msg, ok := c["message"].(map[string]any); ok {
								if txt, ok := msg["content"].(string); ok {
									content.WriteString(txt)
								}
							}
						}
					}
					if _, ok := chunk["usage"]; ok {
						sawUsage = true
					}
				}
			}
			if werr := writeLine(line); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	if !sawDone {
		if !sawUsage {
			if err := writeLine(usageChunk(id, model, created, promptTokens, content.String())); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
	}
	return nil
}

// PromptTokensFromBody 从请求 body 估算 prompt_tokens：messages 内容字符总数 /4（至少 1）。
// content 兼容字符串与数组（多模态 text 块）两种形态；解析失败/无 messages 时返回 1。
func PromptTokensFromBody(body []byte) int {
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return 1
	}
	chars := 0
	for _, m := range req.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			chars += len([]rune(s))
			continue
		}
		var blocks []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &blocks) == nil {
			for _, b := range blocks {
				chars += len([]rune(b.Text))
			}
		}
	}
	return usageTokenEstimate(chars)
}

// usageTokenEstimate 按字符数 /4 估算 token 数（向上取整，至少 1）。
// 上游不提供 token 数时的本地近似（R1）。估算值 >0 则填，否则填 1，
// 避免 0 被聚合器视为「未调用」。
func usageTokenEstimate(chars int) int {
	if chars <= 0 {
		return 1
	}
	n := (chars + 3) / 4
	if n < 1 {
		return 1
	}
	return n
}

// buildUsage 构造 OpenAI 标准 usage 结构（R1）：prompt/completion = 字符数/4，
// total = 两者之和，cached_tokens 恒 0。
func buildUsage(promptTokens int, completion string) map[string]any {
	p := promptTokens
	if p < 1 {
		p = 1
	}
	c := usageTokenEstimate(len([]rune(completion)))
	return map[string]any{
		"prompt_tokens":     p,
		"completion_tokens": c,
		"total_tokens":      p + c,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 0,
		},
	}
}

// attachUsage 把估算的 usage 挂到聚合响应（resp 已有 usage 则不覆盖）。
func attachUsage(resp map[string]any, promptTokens int, completion string) map[string]any {
	if _, ok := resp["usage"]; ok {
		return resp
	}
	resp["usage"] = buildUsage(promptTokens, completion)
	return resp
}

// extractContent 从聚合响应中取 message.content（usage 估算用）。
func extractContent(resp map[string]any) string {
	choices, ok := asSlice(resp["choices"])
	if !ok || len(choices) == 0 {
		return ""
	}
	c, ok := choices[0].(map[string]any)
	if !ok {
		return ""
	}
	msg, _ := c["message"].(map[string]any)
	content, _ := msg["content"].(string)
	return content
}

// usageChunk 构建 [DONE] 前注入的带 usage 的流式 chunk（OpenAI 标准：
// choices 为空数组）。id/model/created 尽量沿用上游流中的值。
func usageChunk(id, model string, created float64, promptTokens int, completion string) string {
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": int64(created),
		"model":   model,
		"choices": []any{},
		"usage":   buildUsage(promptTokens, completion),
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		// 序列化失败（理论上不可能）→ 退化为空，不阻塞流。
		return ""
	}
	return "data: " + string(raw) + "\n\n"
}
