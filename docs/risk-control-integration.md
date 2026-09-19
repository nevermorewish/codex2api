# 风控中心集成

## 自定义模型审计（2026-09-19 追加）

按 `风控.png` 的审计池、审计策略、自定义审核提示词交互独立实现，入口为 **风控中心 → 自定义模型审计**（`/admin/risk-control/model-audit`）。这不是旧 Prompt 检查的跳转链接；配置直接用于风控服务的真实请求判定。

### 使用方式

1. 新增审计节点，填写名称、OpenAI 兼容 Base URL（根地址或以 `/v1` 结尾）、模型名称、API Key、超时及单片上限。模型名称完全自定义，不受网关上游模型目录限制；服务端调用 `/v1/chat/completions`。
2. 多节点按列表顺序运行，可启停、编辑、删除、上移/下移；连接失败、HTTP 错误、无效结果会尝试下一节点。节点超时 100–120000 ms，单次完整审核总预算不超过 120 秒。
3. 编写共享 system 提示词（1–20000 字符），可恢复默认模板或切换九类风险分类模板。待审文本经过 XML 转义后放入 user 消息的 `<user_input>`，不执行模型工具。
4. 选择“自定义对话模型”引擎，启用内容审核，选择“关键词 + 审核 API”或“仅审核 API”，按需选择观察或前置阻断模式。**仅关键词策略会跳过模型调用。** 原 Moderations 配置完整保留，切回对应引擎即可继续使用；原 Prompt 检查始终独立运行。
5. 点击页头“保存配置”使真实请求生效。节点连接测试与“试审”使用当前草稿，即使尚未保存也可以测试。测试消耗所配模型的额度，但不写业务日志、Hash、封禁或邮件。

### 返回契约与策略

```json
{"risk":"unsafe","confidence":0.85,"categories":["violence"],"reason":"简要判定依据"}
```

- `risk` 为 `safe` / `controversial` / `unsafe`，`confidence` 为 0–1 的数字。非分类模式也接受仅有 `confidence` 的返回，按风险概率阈值处理。
- 默认阻断阈值 0.7，标记阈值 0.4；要求 `0 ≤ 标记 ≤ 阻断 ≤ 1`。safe 放行；unsafe 达到阻断阈值时阻断；controversial 或未达到阻断阈值但达到标记阈值时仅标记。观察模式不因模型结果阻断。
- 分类模式支持 violence、illegal、sexual、pii、self_harm、unethical、political、copyright、jailbreak，只处理选中的分类。非 safe 结果缺分类或含未知分类视为无效响应，而不是自动放行。
- 可配置审计失败时放行；默认关闭该选项，所有节点失败时前置模式返回 503，不增加违规次数。保存安全事件的开关也控制审核错误日志。
- 范围、采样、Worker、观察队列、拦截状态码与提示文案复用风控配置；分组仍按调用方 Key 的允许分组匹配。观察队列仍是有界内存队列，不宣称持久化。
- 自定义模型当前审核本轮提取的用户文字，规范化后最多 400000 字符；按启用节点最小单片上限分片。它不审核生成后的媒体或回答，也不将图片直接发送给自定义模型。纯图片无文字输入按审核错误策略处理。
- 仅标记的结果不加入阻断 Hash。自定义模型的 Hash 同时关联提示词、分类、阈值和节点配置；策略变更后不会错误复用旧判定。观察模式中的高风险结果仍可写入 Hash，开启历史 Hash 后的重复命中仍遵循原拦截语义。
- 审核记录展开项显示实际节点、模型、风险、置信度、分类、脱敏原因及分片数量。节点密钥读取时不回显，更新按稳定节点 ID 保留，显式清除或删除节点才移除；调整节点顺序不会串用密钥。

### 本次验证

- 定向 Go 测试覆盖自定义模型与 system 提示词实际发送、输入标签转义、节点故障切换、Unicode 分片、阈值和分类、观察/失败策略、无效/截断 JSON、禁止跟随重定向、密钥按 ID 保留/清除、配置持久化、试审无业务副作用、仅标记不写阻断 Hash、策略变更隔离旧 Hash。
- 前端类型检查、生产构建和风控守卫测试通过；使用共享 Select、Switch、DraftNumberInput 和确认对话框，三语文案齐全。
- 使用独立 SQLite 和本地模型桩验证：页面新增节点、编辑自定义提示词、未保存试审、保存后重载、节点连接测试、节点排序与失败切换、分类关闭后放行；实际 `/v1/messages` 请求由自定义模型判定并返回 403，日志保留审核模型与依据。测试未使用真实供应商密钥。
- 最新前端全量测试 283 项：281 通过，2 项仍为后文列出的既有 Settings/RequestLifecyclePanel 失败；新增风控测试 4 项全部通过。1500px 桌面、390px 移动端和中英繁三语页面无整页横向溢出、无运行时 JavaScript 错误。截图保存在本地 `artifacts/risk-control-verification/model-audit.png` 与 `model-audit-mobile.png`。

配置仍存于原 `risk_control_config` JSON，新增字段为 `audit_engine` 和 `model_audit`；不新增数据表。老配置自动采用原 Moderations 引擎，默认行为保持不变。

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
