# 进度与交接 — TG 桥

日期:2026-10-20
状态:v1 已部署并启动(香港服务器,C 路线公开凭据,面板 18080),
剩余:在网页里完成 Telegram 授权(填手机号 → 收码 → 完成)。

## 凭据决策记录(2026-10-20)

my.telegram.org 限流始终无法通过(多网络出口重试无果),按用户拍板改用
C 路线:Telegram Desktop 开源客户端公开的 api_id=2040 /
api_hash=b18441a1ff607e10a989891a5462e627 跑 MTProto userbot。
风险已在对话中告知(公开凭据滥用有批量处置先例,低频私聊翻译属低风险,
但高于专属凭据)。**回退路径:借道申请成功拿到专属凭据后,改
config.yaml 两行 + systemctl restart tgbridge 即可,session 无需重登。**

运维教训:该服务器对短窗口内多次 SSH 密码认证有防爆破封禁(fail2ban
类,观察约 10 分钟自动解封)。远程操作必须合并为单连接执行。

## 部署状态(香港服务器)

- 服务器:<your-server>:<ssh-port>(root 登录),Ubuntu 24.04 x86_64。
- 安装位置:/opt/tgbridge/{tgbridge(二进制,700), config.yaml(600),
  env(面板密码,600), data/}。
- systemd 单元:/etc/systemd/system/tgbridge.service,EnvironmentFile=
  /opt/tgbridge/env;服务运行中(active),Telegram 已授权在线。
- 面板:http://<your-server>:18080。**8080 常被既有服务(如 docker-proxy)
  占用,面板默认用 18080;云安全组需放行 18080/TCP**。
- 首次 Telegram 登录为网页向导(/api/auth/state + code/verify/password),
  无需 TTY/SSH 交互;session 持久化,重启免登。
- 深圳侧网络注意:FlaClash TUN 的 MTU 必须设为 1500(9000 会导致 SSH
  握手大包黑洞,连接秒断);SSH 操作需合并为单连接(服务器有防爆破
  限流,短窗多次连接会被封约 10 分钟)。
- DeepSeek key 验证通过(2026-10-20):deepseek-chat 实际路由至
  V4.1-Flash(context 1M),OpenAI 兼容字段与客户端解析匹配
  (prompt_cache_hit_tokens / prompt_tokens_details 双字段)。
- 服务器无 nginx/caddy:面板当前为 HTTP 明文 + 密码,建议补 TLS 反代。

### 完成授权(用户操作,浏览器内)

1. 打开面板地址,输入面板密码(/opt/tgbridge/env)。
2. 向导自动出现:输入 Telegram 账号手机号(国际格式,输入框不回填任何
   存值)→ 点「发送验证码」→ 手机(TG 应用内或短信)收到新码 → 填入 →
   验证。开了两步验证会再要求输云密码。
3. 完成后页面自动刷新进入聊天界面;session 落盘,重启免登。

### v1.1 功能(2026-10-20 夜)

- 已读未读:面板侧未读徽标(localStorage 持久);打开对话即向 Telegram
  发送 readHistory,对方能看到已读回执。
- 统计页 /stats:服务端渲染,LLM 调用/tokens/缓存命中率/翻译延迟
  P50/P95/每对话聚合/消息状态分布。
- UI 对齐 Telegram Desktop 暗色配色(背景 #0e1621、侧栏 #17212b、
  气泡 #182533/#2b5278),圆形字母头像(按 peer 稳定生成色相)、
  绿色未读徽标、绿色开关、消息进入动画。
- 列表项昵称/@用户名/access hash 从更新批次的 users 列表透传入库。
- 静态资源标准缓存:内容 md5 ETag + 304 + max-age=300。
- 修复:翻译开关双触发(手动 click 与 label 原生转发叠加,导致状态
  回跳)与点击开关误开对话(事件冒泡)——开关改为原生 change + 乐观更新。

### 启动 / 重启(运维)

```
systemctl start|stop|restart tgbridge   # 常规
journalctl -u tgbridge -f               # 看日志
```
更新二进制:本地 `GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w"
-o tgbridge ./cmd/tgbridge` 后上传覆盖 /opt/tgbridge/tgbridge 并 restart。

**自启**:unit 已 `systemctl enable`(开机自启)+ `Restart=always`
(任何退出 5 秒内自愈)。教训:2026-10-20 服务器重启一次,当时未
enable 导致服务缺席两天——任何新部署完成都必须 enable --now。
离线期间(TG 网关未运行时)到达的消息不会被补收,面板里没有对应
记录,需在 TG 客户端查看。

## 已完成

| 模块 | 文件 | 说明 |
|---|---|---|
| 配置 | internal/config | yaml + env 密码覆盖;密码为空拒绝启动 |
| 存储 | internal/store | SQLite(WAL),chats/messages/glossary/llm_calls;幂等唯一索引(部分索引,0 id 行不受约束) |
| LLM 客户端 | internal/llm | OpenAI 兼容;解析 DeepSeek prompt_cache_hit_tokens;端点内重试 + 跨端点降级 |
| 上下文引擎 | internal/ctxengine | L0-L4 分层组装;append-only 滑窗;水位压缩(保留比例);增量结构化摘要;CJK 判定;长文分块 |
| 管道 | internal/pipeline | per-chat 串行 worker;入站/出站双向;幂等去重(含服务器回推自发消息);队列背压降级;SSE 总线;崩溃恢复(pending 重投) |
| MTProto 网关 | internal/tggw | gotd:session 登录(首次终端交互)、更新归一化(四类更新)、以本人身份发送、FLOOD_WAIT 单次重试 |
| Web | internal/web + assets | 密码登录(HMAC cookie);聊天 API;SSE 流;管理(对话列表/启用开关/统计);前端无框架,响应式双端 |
| 主程序 | cmd/tgbridge | 装配 + 信号优雅退出 |

## 验证记录

- `go build ./...`、`go vet ./...` 通过。
- `go test ./...` 全绿:ctxengine(语言判定/滑窗往返/分块/水位压缩/分层组装/术语层)、llm(缓存字段解析/重试/降级/全失败)、store(幂等与修订/分页/窗口顺序)。
- Linux 交叉编译通过(modernc.org/sqlite 纯 Go 无 CGO)。
- 未验证:真实 Telegram 连接、真实 DeepSeek 调用(需密钥)。

## 剩余待办

1. 用户在网页完成 TG 授权(见「完成授权」),然后从网页发第一条中文
   验证端到端翻译链路。
2. my.telegram.org 借道申请继续挂着(朋友家/公司网络/手机流量,谁申请
   都行,验证码在账号手机上);拿到专属凭据后按「凭据决策记录」回退。
3. 建议:面板补 TLS 反代;root 密码登录改 SSH key;DeepSeek key 若有
   泄露疑虑在后台重置。

## 已知边界(v1)

- 非文本消息(语音/图片无 caption)不入库不入翻译流;有 caption 的媒体翻 caption。
- 对方编辑消息按新修订行入流(原文条目保留在上方,带"翻译中/已编辑"语义)。
- glossary 数据层与引擎支持,管理 UI 未做(手工 INSERT 即生效)。
- 用户在 Telegram 客户端里直接发的消息不翻译、不编辑(记录为上下文);
  需要翻译请从网页端发送。
- 出站翻译失败不发送,原文回填输入框;入站翻译失败显示原文并标注。

## v1.1 候选

- 术语表管理界面;统计页成本曲线(接口 /api/stats 已有原始数据)。
- 语音消息转写翻译(Whisper 系 API)。
- 移动端 PWA(添加主屏图标)。
