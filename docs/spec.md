# TG 桥 — 设计规格

双语私聊实时翻译网关：中文 ⇄ 乌克兰语/英语，LLM 驱动，以个人 Telegram
账号收发消息，网页端为聊天主界面（手机/电脑双端响应式）。

## 1. 目标与非目标

目标：
- 使用者在网页里用中文聊天，对方收到以使用者本人账号发出的乌克兰语；
  对方发来的乌克兰语/英语（含夹俄语）在网页中以「原文 + 中文译文」实时呈现。
- 翻译依赖会话级上下文（滑窗 + 滚动摘要），非逐句孤立翻译。
- 翻译按对话单独启用，默认全部关闭。
- 部署于香港服务器，7×24 在线，单 Go 二进制 + SQLite。

非目标（v1 不做）：
- 语音消息转写翻译（非文本消息在网页显示占位，caption 除外）。
- 群聊多人多语言路由。
- 术语表的管理界面（数据层与上下文引擎已支持，UI 后续版本补）。

## 2. 产品形态

事实约束：Telegram 官方客户端无插件 API；Bot API 账号无法读取两人私聊。
因此：MTProto userbot（个人账号）+ 自建网页聊天界面。

- 出站：网页输入中文 → LLM 翻乌克兰语 → 以本人身份 MTProto 直接发送。
  Telegram 侧从未出现中文，无编辑、无闪烁。中文原文仅存本地数据库。
- 入站：MTProto 收到对方消息 → LLM 翻中文 → SSE 推送到网页聊天流，
  原文与译文对照显示。Telegram 官方客户端仍在，可随时查看原始对话。
- 启用粒度：按对话（人）单独开启。未启用的对话消息仅落库、不翻译、
  不出现在聊天流。发现新对话即列入管理列表，等待手动启用。

已知风险：userbot 自动化在技术层面违反 Telegram ToS。翻译用途的实际
封号案例极少，缓解措施见 §8。

## 3. 技术栈（Go 全栈）

| 层 | 选型 | 理由 |
|---|---|---|
| MTProto | gotd/td + gotd/contrib | Go 生态唯一成熟 MTProto 实现，contrib 提供 session 存储、flood-wait 中间件 |
| 存储 | modernc.org/sqlite | 纯 Go 无 CGO，交叉编译零负担；WAL 模式 |
| LLM 客户端 | 自写轻量 OpenAI 兼容 HTTP client | 需读取 DeepSeek 特有 usage.prompt_cache_hit/miss_tokens 落库观测 |
| 实时推送 | SSE（Server-Sent Events） | 推送为纯单向（服务器→浏览器），EventSource 原生自动重连，少一个 WebSocket 依赖 |
| Web | 标准库 net/http (Go 1.22+ ServeMux) + html/template + embed.FS | 无外部前端依赖，单二进制内嵌全部静态资源 |
| 鉴权 | 密码登录 + HMAC 签名 session cookie | 面板暴露公网，所有路由（含 SSE）必须过鉴权 |
| 配置 | env + yaml，启动加载后不可变 | 密钥不落仓库 |
| 日志 | log/slog JSON | 结构化，便于服务器采集 |
| 并发模型 | goroutine + channel；per-chat 串行 worker | 见 §6 |

单二进制，`go build` 产出 Linux amd64（服务器）/ Windows（本地开发）。

## 4. 数据模型（SQLite）

```sql
CREATE TABLE chats (
  id           INTEGER PRIMARY KEY,
  peer_type    TEXT NOT NULL,             -- 'user'
  tg_user_id   INTEGER NOT NULL,
  access_hash  INTEGER NOT NULL DEFAULT 0,
  username     TEXT,
  title        TEXT,
  persona      TEXT NOT NULL DEFAULT '',  -- 语气/身份说明，注入 L1
  summary      TEXT NOT NULL DEFAULT '',  -- 滚动摘要，注入 L2
  window       TEXT NOT NULL DEFAULT '',  -- 滑窗序列化（消息 id 列表）
  enabled      INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  UNIQUE(peer_type, tg_user_id)
);

CREATE TABLE messages (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  chat_id       INTEGER NOT NULL REFERENCES chats(id),
  tg_msg_id     INTEGER NOT NULL,
  edit_rev      INTEGER NOT NULL DEFAULT 0,   -- 入站：对方编辑消息时 +1 重翻
  direction     TEXT NOT NULL,                -- 'in' | 'out'
  lang_detected TEXT,
  source_text   TEXT NOT NULL,
  target_text   TEXT,
  status        TEXT NOT NULL DEFAULT 'pending',
                -- pending / translating / done / failed / skipped
  tokens_in     INTEGER, cached_tokens INTEGER, tokens_out INTEGER,
  latency_ms    INTEGER,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  UNIQUE(chat_id, tg_msg_id, edit_rev)
);

CREATE TABLE glossary (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  chat_id  INTEGER NOT NULL REFERENCES chats(id),
  src_term TEXT NOT NULL,
  dst_term TEXT NOT NULL,
  note     TEXT,
  UNIQUE(chat_id, src_term)
);

CREATE TABLE llm_calls (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  message_id        INTEGER NOT NULL,
  kind              TEXT NOT NULL,        -- 'translate' | 'summarize'
  endpoint          TEXT NOT NULL,
  model             TEXT NOT NULL,
  prompt_tokens     INTEGER NOT NULL,
  cached_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL,
  duration_ms       INTEGER NOT NULL,
  error             TEXT,
  created_at        TEXT NOT NULL
);
```

消息先落库为 pending 再进翻译流程；崩溃后按 pending 重投，幂等键
(chat_id, tg_msg_id, edit_rev) 防重复消费与重复发送。

## 5. 上下文管理（核心设计）

### 5.1 Prompt 分层

按变化频率从慢到快排列，最大化 DeepSeek 自动前缀缓存命中率
（命中部分价格约为未命中的十分之一）：

```
L0  系统层：人设、输出格式约束、方向规则        变化：几乎不变
L1  术语表 + 双方 persona                     变化：手动编辑时
L2  会话滚动摘要                               变化：压缩事件时
L3  近期消息滑窗（双语对照）                   变化：append-only
L4  待翻译消息                                 变化：每次请求
```

映射为 OpenAI messages 数组：

```json
[
  {"role":"system","content":"L0 + L1 + L2"},
  {"role":"user",  "content":"对话记录：\n[L3 双语对照块，append-only]"},
  {"role":"user",  "content":"翻译以下这条新消息：[L4]"}
]
```

滑窗每条消息双语对照：

```
[A] Привіт, о котрій годині завтра прибудеш? → 明天几点到？
[B] 你好，我十点到。 → Привіт, я прибуду о десятій.
```

带旧译文是为了让模型看到自己此前的译法，术语与人称不漂移；
跨语言指代消解必须同时看到两种语言的文本。

### 5.2 滑窗 append-only（与 KV 前缀缓存的耦合）

关键工程点：若滑窗按「固定条数」维护，每条新消息从头部弹出一条，
L3 前缀整体失效。正确做法：

- L3 只追加，直到累计 token 超过水位（默认 4000）。
- 触发 compaction：滑窗前半增量并入 L2 摘要，滑窗保留后半，
  一次性完成。L2 与 L3 同时失效一次，失效成本合并为单事件。
- 稳态下每次请求未命中 token 仅 ≈ L4 + 上次请求以来的新增消息。

### 5.3 滚动摘要

- 增量压缩：新摘要 = LLM(旧摘要 + 本次移出滑窗的消息)，不重读全量。
- 结构化模板：{话题状态 / 双方立场与情绪 / 关键事实（数字、日期、
  金额逐字保留）/ 约定与未决事项}。
- 摘要 token 封顶（默认 400），超限由 LLM 取舍，数字与专名优先。
- 摘要失败不截断滑窗：保留旧摘要与原滑窗，下次水位再试，不丢数据。

### 5.4 语言路由与跳过

- 出站（网页输入）：按 Unicode 区段估算 CJK 占比，主体为中文才翻译；
  纯目标语/纯符号/超短消息 status=skipped 直通（LLM 零调用）。
- 入站：主体非中文才翻译；对方真发中文则直接显示不调用 LLM。
- 语言判定不单独调 LLM，靠字符统计，零成本。

### 5.5 触发与预算参数（可配置）

| 参数 | 默认 | 说明 |
|---|---|---|
| WINDOW_WATERMARK | 4000 tok | 超限触发 compaction |
| WINDOW_KEEP_RATIO | 0.5 | 压缩后滑窗保留后半 |
| SUMMARY_MAX | 400 tok | 摘要封顶 |
| IDLE_RECOMPACT | 12h | 静默超时后下次翻译前先压缩 |
| MAX_INPUT_MSG | 500 字 | 超长消息分块翻译后拼接 |

token 计数在本地用启发式估算（CJK≈1/字，其他语种≈1.3/词），仅用于
水位判断，允许 ±20% 偏差；真实用量以 LLM 响应 usage 字段落库为准。

### 5.6 观测

每次 LLM 调用落 llm_calls（cached/prompt/completion tokens、延迟、
端点、kind）。管理接口输出：每对话成本、缓存命中率、延迟分位。
命中率低于 60% 视为组装策略回归，需要排查。

## 6. 消息管道

```
MTProto update ──▶ dispatcher ──▶ per-chat channel ──▶ chat worker（串行）
网页 POST 发送 ──▶ dispatcher ──┘
                     │ allowlist：未启用的对话只落库不翻译
                     │ 幂等去重（chat_id, tg_msg_id, edit_rev）
                     ▼
              落库 pending ──▶ 语言判定 ──▶ 上下文组装 ──▶ LLM 调用
                     │                                         │
                     │                     成功：出站=MTProto 发送 / 入站=SSE 推送
                     ▼                     失败：重试(退避)→ fallback 端点 → 标记 failed
              状态机更新 done/failed/skipped
```

- 出站：翻译成功才发送；LLM 失败则网页回显错误，不发送半截内容。
- 入站：翻译失败仍推送原文（标注「翻译失败」），不阻塞阅读。
- 入站编辑：对方编辑消息按 edit_rev+1 重新翻译并更新网页流。
- 保序：per-chat 单 worker 从 channel 顺序消费；对话间并发。
- 背压：channel 满则仅落库标记 skipped，防止洪水占死内存。

## 7. LLM 客户端与端点

- OpenAI chat completions 兼容协议，base_url/model/key/endpoint 全可配。
- 多端点列表 + 失败切换：primary（DeepSeek）超时/限流 → fallback
  （任意 OpenAI 兼容：GLM/Qwen/GPT），恢复后切回。
- 超时 15s，重试 2 次（指数退避 1s/4s），重试不改变前缀（保缓存）。
- 采样参数进配置；初始值按 DeepSeek 官方翻译场景建议，上线按实测调。
- 流式：v1 不用。出站需完整译文才能发送，入站推送同样一次性完成。

## 8. 风险登记

| 风险 | 等级 | 缓解 |
|---|---|---|
| userbot 违反 TG ToS，理论封号 | 低概率 | 单一低频用途；session 泄漏立即在 my.telegram.org 撤销 |
| 网页公网暴露 | 安全 | 强制密码登录，HMAC session cookie（含过期），SSE 也过鉴权；建议前置 nginx/caddy 加 TLS |
| session 文件 = 账号钥匙 | 安全 | 仅存服务器，权限 600，不入仓库 |
| 摘要丢关键事实 | 质量 | 结构化摘要 + glossary 双兜底；数字逐字保留约束 |
| DeepSeek 缓存策略变化 | 成本 | llm_calls 记录真实命中率，策略失效可观测 |
| 双端体验不一致 | UI | 响应式单一模板，<768px 单栏切换、≥768px 双栏 |

## 9. 里程碑

- M1 管道骨架：登录、allowlist、出站组装→翻译→发送、入站→翻译→
  SSE 推送、SQLite 落库。单句直翻（无 L2/L3），端到端可用。
- M2 上下文引擎：L0–L4 组装、滑窗水位压缩、增量摘要、缓存对齐。
  组装器纯函数化，单测覆盖（水位/截断/幂等/语言判定）。
- M3 网页界面：登录、对话列表+启用开关、聊天流双语对照、发送框，
  手机/电脑响应式。
- M4 观测与打磨：成本、命中率、延迟分位接口；崩溃恢复演练。

## 10. 部署

- 香港服务器 Linux amd64；systemd 常驻。
- 首次登录交互式（手机号 + 验证码）：建议本地 Windows 跑一次生成
  session 文件后上传服务器，或 SSH 终端内完成。
- 配置文件 + env 密钥；`go build` 交叉编译单文件发布。
- 备份：SQLite 文件 + session 文件（含全部状态）。
- 生产建议：前置 caddy/nginx 终结 TLS，源站只听 127.0.0.1。
