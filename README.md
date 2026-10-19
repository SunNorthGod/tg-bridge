# TG 桥 (tg-bridge)

Telegram 双语私聊实时翻译网关:中文 ⇄ 乌克兰语/英语,LLM 驱动,
以个人 Telegram 账号收发消息,网页面板为聊天主界面(手机/电脑响应式)。

你在网页里打中文,对方在 Telegram 里收到以你本人账号发出的乌克兰语;
对方发来的乌克兰语/英语(含夹俄语)在网页中实时显示原文与中文译文对照。

## 特性

- **隐形桥**:MTProto 个人账号直连,消息以你本人身份发出,对方无感
- **会话级上下文翻译**:滑窗 + 滚动摘要,不是逐句孤立翻译;代词、称呼、
  术语跨消息保持一致
- **前缀缓存对齐**:prompt 分层按变化频率排列,滑窗 append-only,稳定
  吃满 DeepSeek 自动前缀缓存(命中部分约十分之一价格),命中率在统计页
  实时可见
- **按对话启用**:默认全部关闭,在列表中单独打开
- **已读回执**:面板内读消息,TG 侧同步已读,对方能看到
- **首次登录零 SSH**:session 未授权时服务不退出,在网页向导里完成
  手机号→验证码→(2FA) 登录,适合 systemd 常驻
- **观测**:每次 LLM 调用的 tokens/缓存命中/延迟落库,`/stats` 页展示
  P50/P95、每对话聚合、消息状态分布
- 单 Go 二进制,内嵌前端,SQLite 单文件存储

## 架构

```
对方(Telegram)
    ↕ MTProto (gotd/td,个人账号)
┌──────────────────────────────────────────────┐
│ tgbridge 单进程                               │
│  网关 ─ 管道(per-chat 串行 worker)─ 上下文引擎 │
│                │                │              │
│              SQLite          LLM(OpenAI 兼容 │
│            (WAL)             /重试/多端点降级)  │
│                │                               │
│              Web 面板(登录/SSE 推送/统计)      │
└──────────────────────────────────────────────┘
    ↕ HTTP + SSE
你(浏览器:手机/电脑)
```

## 快速开始

要求:Go 1.22+,Telegram api_id/api_hash
([my.telegram.org](https://my.telegram.org)),任一 OpenAI 兼容 API key。

```
go build -o tgbridge ./cmd/tgbridge
cp config.example.yaml config.yaml   # 填 api_id / api_hash / llm 端点
export TGBRIDGE_PASSWORD=面板访问密码
./tgbridge -config config.yaml
```

浏览器打开 http://localhost:8080 → 输面板密码 → 网页里完成 TG 登录
(手机号→验证码→可选 2FA)→ 在 TG 客户端里给对方发条消息让对话出现
→ 列表里打开该对话的翻译开关 → 开始聊天。

## 上下文管理

翻译请求的 prompt 分层(变化频率从慢到快):

```
L0 系统层(人设/输出格式/方向规则)   几乎不变
L1 术语表 + persona                 手动编辑
L2 会话滚动摘要(结构化模板)         压缩事件时
L3 近期消息滑窗(双语对照)          append-only
L4 待翻译消息                       每次请求
```

滑窗只追加,超过 token 水位(默认 4000)触发一次压缩:前半增量并入摘要、
后半保留,摘要与滑窗的缓存失效合并为单事件。摘要为结构化模板,数字、
日期、金额逐字保留。每次调用的真实 cached/prompt/completion tokens 落
`llm_calls` 表。

## 部署

```
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o tgbridge ./cmd/tgbridge
```

上传二进制 + config.yaml 后用 systemd 常驻(模板见 docs/PROGRESS.md);
面板密码通过 `TGBRIDGE_PASSWORD` 环境变量或 systemd `EnvironmentFile`
注入。生产建议前置 caddy/nginx 终结 TLS,源站只听 127.0.0.1。

## 风险须知

MTProto 个人账号自动化(userbot)在技术上违反 Telegram ToS,存在账号
受限的理论风险;本项目为低频私聊翻译用途。请自行评估,勿用于批量操作
或营销场景。session 文件即账号凭证,注意权限与备份。

## 项目结构

```
cmd/tgbridge/        入口
internal/config      配置加载(env 密码覆盖)
internal/store       SQLite(chats/messages/glossary/llm_calls)
internal/llm         OpenAI 兼容客户端(重试+跨端点降级+缓存字段解析)
internal/ctxengine   上下文引擎(L0-L4/滑窗/压缩/摘要/语言判定)
internal/pipeline    per-chat worker/幂等/SSE 总线/崩溃恢复
internal/tggw        gotd 网关(网页授权状态机/更新归一化/发送/已读)
internal/web         面板(API/SSE/统计页/鉴权)+ 内嵌静态资源
docs/               设计规格(spec.md)与运行交接(PROGRESS.md)
```

## License

MIT
