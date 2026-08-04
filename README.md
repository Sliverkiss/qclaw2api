# qclaw2api

> 河流从不拒绝汇入的溪流，只是让每一滴水都流向大海。
> 这个项目也一样——把不同的对话能力，汇聚成一个统一的出口。

一个轻量、稳定的 **OpenAI 兼容网关**。它将底层对话服务封装为标准 API，
让你的应用可以用一种语言，与多种模型对话。

```
你的应用 → (Bearer YOUR_KEY) → qclaw2api
  ├─ /v1/chat/completions   流式 / 非流式对话
  ├─ /v1/models             模型列表
  └─ /healthz, /status      健康检查 / 账号状态
```

## ✨ 特性

- 🔑 **多账号池** — 多个账号自动轮换，一个挂掉自动切换下一个，服务不断流
- 🛡️ **智能冷却** — 遇到限流、额度不足、会话失效，自动隔离故障账号
- ⏰ **自动恢复探测** — 每天 0 点（中国时区）自动测试冷却中的账号，恢复后重新入池
- 📡 **流式 + 非流式** — 标准 SSE 透传，保留推理过程（reasoning_content）
- 📊 **OpenAI 标准 usage** — 每次响应都带 `usage` 字段，兼容各类聚合监控面板
- 🏗 **Docker 一键部署** — 多阶段构建 + 健康检查 + 非 root 运行

## 🚀 快速开始

### 1. 准备

```bash
git clone https://github.com/Sliverkiss/qclaw2api.git
cd qclaw2api
cp config.example.json config.json
# 编辑 config.json 或使用环境变量覆盖（见下表）
```

### 2. 添加账号

```bash
./login.sh
# 1) 打开打印的授权链接，扫码完成登录
# 2) 把回调地址（含 code=）粘贴回终端
# 3) 自动完成登录 → 获取凭证 → 落盘凭证文件到 auths/
```

> 新账号首次登录会提示「需再登录一次」，重跑 `./login.sh` 即可。
> 添加多个账号可以组成账号池，自动轮换负载。

### 3. 启动服务

```bash
export QC2A_API_KEY=your-strong-api-key   # 必设：对外鉴权 key
docker compose up -d --build
```

### 4. 验证

```bash
curl http://127.0.0.1:7865/healthz                                   # → ok
curl http://127.0.0.1:7865/v1/models -H "Authorization: Bearer YOUR_KEY"
curl http://127.0.0.1:7865/v1/chat/completions \
  -H "Authorization: Bearer YOUR_KEY" -H "Content-Type: application/json" \
  -d '{"model":"default","messages":[{"role":"user","content":"你好"}]}'
```

## ⚙️ 配置说明

| env | 覆盖字段 | 默认 |
|---|---|---|
| `QC2A_LISTEN` | 监听地址 | `:7865` |
| `QC2A_API_KEY` | **对外鉴权 key（必设）** | `change-me` |
| `QC2A_AUTH_DIR` | 账号凭证目录 | `./auths` |
| `QC2A_STATE_FILE` | 账号池状态文件 | `./data/state.json` |
| `QC2A_MODELS_FILE` | 模型列表文件 | `./data/models.json` |
| `QC2A_SOFT_RATE` / `QC2A_ERR_THRESHOLD` / `QC2A_ERR_COOLDOWN` | 冷却参数 | `60s` / `5` / `10m` |
| `QC2A_KEEPALIVE_HOURS` | 每日恢复探测整点 | `[0]` |
| `QC2A_TIMEOUT_SECONDS` | 非对话请求超时 | `120` |

完整 JSON 配置见 `config.example.json`。

## 🔑 可用模型

`default`（自动路由，推荐）/ `pool-hy3-preview` / `pool-deepseek-v4-pro` /
`pool-deepseek-v4-flash` / `pool-glm-5.2` / `pool-glm-5.2-night` / `pool-glm-5.1` /
`pool-kimi-k2.7-code-highspeed` / `pool-kimi-k2.6` / `pool-minimax-m3` / `pool-minimax-m2.7`

> 模型列表在登录时自动拉取写入 `models.json`，随上游变动。

## 🧭 工作原理

```
请求 → 鉴权 → 从账号池挑一个健康账号
  → 转发对话 → 流式透传 / 聚合
  → 出错了？换下一个账号重试（最多 3 次）
  → 额度不足 / 限流？冷却这个账号，明天 0 点再探测
```

- **账号轮换**：当前账号健康就持续复用，出问题自动切换
- **错误分类**：限流（短冷却）、额度不足（等探测恢复）、会话失效（永久停用）
- **重试**：网络抖动换号重试，模型级故障直接返回错误，不误伤账号

## 📝 免责声明

本项目仅供学习研究使用。使用者需遵守相关服务条款，自行承担使用风险。

## License

MIT
