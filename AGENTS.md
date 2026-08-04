# AGENTS.md — qclaw2api 修复执行约束

## 工作目录
- `/root/qclaw2api/` 是唯一工作目录, 只在此目录内创建/修改文件
- SPEC: `/root/qclaw2api/.omh/SPEC.md`

## 禁止修改(生产运行中)
- `/root/qclaw2api/auths/` — 17 个账号凭证, 正在被容器使用
- `/root/qclaw2api/data/state.json` — 账号池状态
- `/root/qclaw2api/config.json` — 部署配置(keepalive_hours 等, 由 Hermes 改)
- `/root/qclaw2api/.env` — 鉴权 key
- 生产容器 `qclaw2api` 不要动(除非 Hermes 明确指示重建)

## 允许修改
- `internal/upstream/sse.go` — usage 估算 + 流式注入(R1)
- `internal/upstream/sse_test.go` / 新增 `usage_test.go` — 单测
- `internal/server/handler.go` — 传输错误不惩罚账号(R2)
- `internal/pool/pool.go` — CooldownCredit 次日 0 点 + 探测失败滚动(R3)
- `internal/scheduler/scheduler.go` — keepalive 失败重冷却(R3)
- `cmd/server/config.go` — keepalive_hours 默认 [0](R3)
- `config.example.json` — keepalive_hours [0](R3)
- `internal/auth/auth.go` — 注释清理(R5)
- `.omh/SPEC.md` — 如有需求澄清可追加

## 测试要求
- 全部用 Go 单测, 不依赖真实账号/网络(测试内用 httptest 或注入假上游)
- `go build ./... && go vet ./... && go test ./...` 必须通过
- 不改动已有测试的既有断言逻辑(除非 SPEC 需求明确要求)

## 语言
- 代码注释/提交信息用中文(与现状一致)
- 提交信息格式: `fix(qclaw): ...`
