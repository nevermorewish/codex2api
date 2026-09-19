# 风控中心集成

## 目标与适配

在左侧新增独立风控中心，保留原提示词过滤引擎。移植 sub2api 的关键词匹配、输入提取、Moderations 分类阈值、观察/前置阻断、历史 Hash、审核记录、采样、分组/模型范围、审核 Key 健康、通知、自动封禁和清理能力。原项目的用户封禁适配为本项目调用方 API Key 风控封禁，不禁用上游供应商账号。

参考项目许可证为 LGPL-3.0；本集成独立实现输入提取与 AC 匹配器，不直接复制其代码。保留普通字符串、忽略大小写、词表顺序优先的语义。关键词只在前置阻断模式工作；keyword_only 未命中立即放行；Hash 检查不受采样率影响。默认关闭，不修改现有提示词过滤配置。

持久化独立配置、日志、Hash 与风控封禁表，兼容 SQLite/PostgreSQL。审核凭证只在管理员提交时接收，读取时仅返回摘要，空凭证输入保留现值，删除必须显式提交。队列有界，运行状态暴露排队/丢弃/错误数量。日志仅存脱敏摘要。

## 验收清单

- [x] 左侧菜单、路由、加载/失败/空状态、响应式风控中心页面。
- [x] 配置保存、恢复读取、验证、凭证保留/删除与连接测试。
- [x] 本地词表、审核 API、模式、阈值、模型/API Key 分组范围与采样。
- [x] HTTP、Messages、Responses、图片/视频文本及 Responses WebSocket 前置检查。
- [x] 持久日志、筛选分页、统计、Hash 删除/清空、封禁与解封。
- [x] 有界观察队列、清理、通知与运行状态。
- [x] 现有提示词过滤/语义审核入口及原行为保留。
- [x] 单元/集成测试、前端类型检查与构建、页面运行验证（全量回归的既有失败见下文）。

## 运行语义

风控配置关闭后不新增审核；已经封禁的 Key 仍需显式解封。已有提示词过滤继续独立执行。邮件发给管理员配置的收件人（本项目无用户邮箱账户模型）。通知不携带完整提示词或密钥。长输入按参考实现只审核规范化后的前 12,000 字符；该边界在页面明确提示。

### 与参考项目的适配边界

- 风控中心地址：`/admin/risk-control/policy`。子页为 policy、keys、logs、bans、test、prompt；嵌入的原提示词编辑器使用 `/admin/risk-control/prompt/:view`，旧 `/admin/prompt-filter/:view` 仍保留。
- 分组按调用方 Key 的 `AllowedGroupIDs` 与配置分组取交集，不按最终选中的上游账号判断。配置分组为空表示全部；调用方未限制分组时，其空列表不会自动匹配显式配置的分组。模型名单忽略大小写精确匹配。
- 禁用配置后已有封禁仍生效。自动封禁默认关闭；默认总开关也关闭。配置损坏在启动时报错；封禁状态读取异常返回 503；外部审核 API 异常按 fail-open 处理。
- HTTP 重复检查相同正文、模型、端点会复用同一请求内的决定；WebSocket 每轮独立检查。
- Hash 存储使用当前数据库，不增加 Redis 依赖。关键词命中不写入 Hash；API 风险命中写入；Hash 命中不累加封禁计数。观察模式的 Hash 命中仍阻断。
- 关键词命中、日志、违规计数和封禁同步事务提交；邮件与观察审核异步执行。队列限制单任务 256 KiB、总计 32 MiB，重启丢失排队任务，日志和封禁保留。
- 审核 API 使用 OpenAI 兼容 Moderations 格式，图片审核发送提取的第一张图片；图片/视频生成接口检查文字提示词，不审核生成后的媒体。
- 管理员接口不返回完整审核 Key 或 SMTP 密码。密钥需要可逆读取以调用服务，保存在数据库配置中；部署时应保护数据库文件、访问权限及备份。不要将配置数据库加入代码库。
- 配置热更新、审核 Key 冷却状态及运行计数属于本进程；多副本部署需要统一发布配置/重启其他副本，队列和运行计数不做跨实例聚合。

## 验证记录（2026-09-19）

### 自动测试

在项目根目录执行：

```powershell
go test ./security/riskcontrol ./database ./admin ./proxy -run '^(TestRiskControl|TestPromptFilter|TestReviewPromptFilter)' -count=1 -timeout 120s
go vet ./security/riskcontrol
```

结果均通过。覆盖 AC 匹配/Unicode/顺序、输入提取、零采样、模型及调用 Key/分组范围、无效配置、管理员鉴权、密钥保留/清除、审核调用及错误放行、Key 重试冷却、观察队列、Hash、封禁/解封、通知失败、HTTP 各适配入口与真实 WebSocket 两轮检查。

`TestRiskControlPostgresIntegration` 在一次性 PostgreSQL 16 容器中实际通过，验证配置、8 个并发事件事务、封禁、解封、Hash、统计和清理。设置 `RISK_TEST_POSTGRES_DSN` 指向一次性测试数据库后可重跑；不要指向生产库。未设置该变量时该用例跳过，其余数据库测试使用临时 SQLite。

前端：`npm run typecheck`、`npm run build`、`node --experimental-strip-types --test src/lib/riskControl.test.mjs` 均通过。Go 主程序构建通过并用于页面验证。当前机器未配置 CGO/gcc，未执行成功 race 检测。

### 页面和实际请求

使用独立临时 SQLite，网关只监听 `127.0.0.1:18087`，审核桩只监听 `127.0.0.1:18088`；没有配置真实供应商或真实邮件凭证。

| 验收项 | 实测结果 |
|---|---|
| 菜单及入口 | 桌面左侧菜单选中风控中心，六个子页正常渲染 |
| 保存与刷新 | 共享下拉切换 off / pre_block，数值 75 / 100 保存后重新加载一致；off 显示“关闭” |
| 未保存更改 | 刷新弹出共享确认框；取消保留修改，确认恢复服务端值 |
| 本地词表 | `contains blocked-fixture` 命中 `BLOCKED-FIXTURE`，测试不写业务日志 |
| 凭证管理 | 保存后输入框清空、只显示掩码；局部配置保存保留凭证；本地连接测试 HTTP 200、健康状态 ok；页面删除成功 |
| 实际拦截/封禁 | `/v1/messages` 两次关键词命中均 403，随后干净输入返回 `risk_key_banned`；页面解封后封禁数归零 |
| API / Hash | 本地审核桩返回 violence=0.99，首请求记录 block，重复请求记录 hash_block，页面删除指定 Hash 后数量归零 |
| 列表失败恢复 | 注入日志接口 HTTP 500，显示持久错误及重试按钮；恢复接口并重试后显示真实记录 |
| 语义审计 | 从风控中心进入原审核配置，日志和规则集切换仍位于 `/risk-control/prompt/...` |
| 语言及布局 | 简中、英文、繁中标题和文案正常；1440px 桌面、390px 移动端六页无整页横向溢出，宽表/导航局部滚动 |
| 浏览器 | 正常页面无 JavaScript 错误；故障注入产生预期 HTTP 500；共享 Modal 存在已有 Radix Description 可访问性警告 |

邮件通道实现 TLS/STARTTLS。`TestRiskControlNotificationSTARTTLS` 使用本地测试证书实际完成 STARTTLS、信封和邮件 DATA 传输，并确认仅包含事件元数据、不携带提示词；另已测试连接失败不会改变阻断决定且增加通知错误计数。真实邮件服务商的投递/收件策略未验收，部署后需用自己的 SMTP 配置确认送达。

截图与测试证据在 `artifacts/risk-control-verification/`：`desktop.png`、`mobile.png`、`backend-tests.txt`、`postgres-tests.txt`、`frontend-regression.txt`、`baseline-backend.txt`、`baseline-frontend.txt`、`baseline-timeout.txt`。

### 全量回归的既有失败

前端全量 `npm test`：282 项，280 通过，2 项失败，均在未修改快照复现：Settings 共享卡片缺渠道标注，以及 `RequestLifecyclePanel.tsx` 仍使用原生 select。新增风控页符合共享控件约束。

后端全量运行存在以下既有失败，已在未修改快照单独重跑复现，未作为本次风控改动修复：

- `TestUpdateSettingsResponseIncludesRetrySettings`
- `TestUpdateSettingsDoesNotPublishContinuousRetryWhenPersistenceFails`
- `TestRoutingSchedulerEvictsSingleLRUEntry`
- `TestCommitResponsesStreamAttemptClearsStagedTurnStateOnReplayFailure`
- `TestContinuousRetryReplaySpillsAndRemovesTemporaryFile`
- `TestContinuousRetryReplayMapsFileErrorsWithoutPath`
- `TestContinuousRetryWSReplayPreservesMessageBoundaries`
- `TestContinuousRetryWSReplayForEachMessageIsReadOnly`
- `TestStage0BlockedMultipartRemovesTemporaryFiles`
- `TestResponsesTurnStateDoesNotRerouteAfterAuthoritativeLimit` 在全量运行超时，在未修改快照 20 秒重跑也超时。

## 部署与回退

1. 先备份现有数据库。先构建前端，再构建 Go 主程序，使嵌入页面与后端版本一致。
2. 启动时自动新增四张独立表：`risk_control_config`、`risk_control_logs`、`risk_control_hashes`、`risk_control_bans`；不改写已有提示词过滤配置。
3. 登录后台进入风控中心。仅关键词模式不需要审核 API Key；API 模式需配置 Base URL、审核模型及审核凭证，先进行连接/内容测试，再启用策略。
4. 需要暂停审核时关闭总开关；如需恢复已封禁 Key，必须在封禁页显式解封。原提示词审计仍按自身设置独立执行。
5. 程序回退时恢复原二进制/前端即可。新增表可保留供再次升级使用，不必删除；如要清除数据，先备份，再仅操作上述四张表。回退至旧版本后新风控封禁门禁也不再执行。
