# QClaw2API

> QClaw（腾讯电脑管家团队的 OpenClaw 商业化封装）的 **OpenAI 兼容反向代理**，Go 语言实现。
> 把 QClaw 的额度（Q 点 + 每日 tokens）与能力（对话 / 生图 / 模型列表）暴露成标准 OpenAI API，支持多账号轮转、自动续期与 Docker 部署。

```
OpenAI 客户端 → (Authorization: Bearer API_KEY) → qclaw2api
   ├─ /v1/chat/completions    → aizone 直连（sk-apiKey + UA + system + 白名单 body）
   ├─ /v1/images/generations  → jprx 4299（提交 → 轮询 → image_url）
   ├─ /v1/models              → jprx 4320（缓存 1h，失败回退静态表）
   └─ /status, /healthz       → 本地
```

## ✨ 功能特性

- 🔐 **微信扫码登录** — `login.sh` 交互式登录：生成授权链接 → 扫码 → 回贴回调 code → 自动落盘 auth 文件
- 🆕 **新用户判定** — `is_new_user=true` 时提示再登录一次转正，避免未激活账号入池
- 🔄 **多账号轮转** — 按 Q 点余额降序选号，冷却/禁用状态机防雪崩
- 🔑 **自动续期** — 每日定时 4058 refresh + 响应头 `X-New-Token` 捕获，JWT 滑动续期
- 📡 **流式 + 非流式** — SSE 透传（保留 `reasoning_content`），非流式聚合 `tool_calls`
- 🖼 **AI 生图** — jprx 4299 提交 → 轮询 → 返回 OpenAI 格式 image_url
- 📊 **积分监控** — `credit.sh` 一键查询全部账号 Q 点 / 每日 tokens
- 🏗 **Docker 部署** — 多阶段构建 + healthcheck + 非 root 运行

## 🚀 快速开始

### 1. 克隆 & 配置

```bash
git clone https://github.com/Sliverkiss/qclaw2api.git
cd qclaw2api
cp config.example.json config.json
# 编辑 config.json 或设置环境变量 QC2A_API_KEY（见下）
```

### 2. 登录 QClaw 账号

```bash
./login.sh
# 1) 打开打印的微信授权链接，手机扫码
# 2) 浏览器跳转后把回调 URL（含 code=）粘贴回终端
# 3) 自动完成 4026 登录 → 4055 取 sk-apiKey → 4110 查积分 → 落盘 auths/qclaw-<uid>.json
```

> 新账号首次登录会提示「需再登录一次转正」，重跑 `./login.sh` 即可。

### 3. 启动服务

```bash
export QC2A_API_KEY=your-strong-api-key   # 必设：用户层鉴权 key
docker compose up -d --build
```

### 4. 验证

```bash
curl http://127.0.0.1:7865/healthz                                   # → ok
curl http://127.0.0.1:7865/v1/models -H "Authorization: Bearer $QC2A_API_KEY"
curl http://127.0.0.1:7865/v1/chat/completions \
  -H "Authorization: Bearer $QC2A_API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"pool-deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}'
```

## ⚙️ 配置说明

| env | 覆盖字段 | 默认 |
|---|---|---|
| `QC2A_LISTEN` | 监听地址 | `:7865` |
| `QC2A_API_KEY` | **用户层鉴权 key（必设）** | `change-me` |
| `QC2A_AUTH_DIR` | 账号凭证目录 | `./auths` |
| `QC2A_STATE_FILE` | 账号池状态文件 | `./data/state.json` |
| `QC2A_HARD_CREDIT` / `QC2A_SOFT_RATE` / `QC2A_ERR_THRESHOLD` / `QC2A_ERR_COOLDOWN` | 冷却参数 | `12h` / `60s` / `5` / `10m` |
| `QC2A_KEEPALIVE_HOURS` / `QC2A_CREDIT_INTERVAL_HOURS` | 续期/积分刷新 | `[4]` / `6` |
| `QC2A_TIMEOUT_SECONDS` | 非对话请求超时 | `120` |
| `QC2A_IMAGE_POLL_INTERVAL` / `QC2A_IMAGE_POLL_TIMEOUT` | 生图轮询 | `3s` / `120s` |

完整 JSON 配置见 `config.example.json`。

## 🔑 可用模型

来自 QClaw 4320 接口（11 个）：

`default`（Auto 智能路由）/ `pool-hy3-preview` / `pool-deepseek-v4-pro` / `pool-deepseek-v4-flash` / `pool-glm-5.2` / `pool-glm-5.2-night` / `pool-glm-5.1` / `pool-kimi-k2.7-code-highspeed` / `pool-kimi-k2.6` / `pool-minimax-m3` / `pool-minimax-m2.7`

## 📁 工具脚本

| 脚本 | 用途 |
|---|---|
| `./login.sh` | 微信扫码登录，落盘 auth 文件 |
| `./credit.sh` | Q 点/额度日报（`-json` 输出原始 JSON） |

## 🧱 架构

```
internal/
├── auth/        # auth 文件解析/原子写回/快照读
├── jprx/        # jprx 网关 HMAC 签名 + 各 cmd 封装 + X-New-Token 捕获
├── pool/        # 账号池状态机（挑选/冷却/禁用/rpm 限流）+ state.json
├── upstream/    # aizone 对话（UA/system/白名单）+ SSE + 4299 生图
├── scheduler/   # 定时积分刷新 + token 续期
└── server/      # OpenAI 兼容路由 + 鉴权 + 错误分类
```

## 📝 免责声明

本项目仅供学习和研究使用。使用者需遵守 QClaw / 腾讯的服务条款，自行承担使用风险。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

MIT
