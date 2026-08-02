// Package upstream 封装 QClaw 对话上游（aizone）与错误分类。
// aizone 脱机直连三要素（SPEC §1.4）：UA OpenAI/JS 6.39.1 + system role + body 白名单。
package upstream

import "encoding/json"

// QClawAllowedKeys 是 aizone body 白名单（SPEC §1.4 的 _QCLAW_ALLOWED_KEYS）。
// 非白名单字段会触发 9002「该功能暂不可用」，故必须严格过滤。
var QClawAllowedKeys = []string{
	"model",
	"messages",
	"max_tokens",
	"max_completion_tokens",
	"stream",
	"temperature",
	"top_p",
	"stop",
	"tools",
	"tool_choice",
	"frequency_penalty",
	"presence_penalty",
	"n",
	"user",
	"seed",
	"logprobs",
	"top_logprobs",
	"response_format",
	"logit_bias",
	"cache_control",
}

// CleanBody 遍历入参 body，只保留白名单字段；非标字段丢弃（不报错，防 9002）。
// 返回清洗后的 body；错误仅发生在 body 非 JSON 时。
func CleanBody(raw []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(QClawAllowedKeys))
	for _, k := range QClawAllowedKeys {
		allowed[k] = true
	}
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		if allowed[k] {
			out[k] = v
		}
	}
	return json.Marshal(out)
}
