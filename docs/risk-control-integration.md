# 风控中心集成

## 当前行为（2026-09-20）

风控中心保留：审核策略、自定义模型审计、审核记录、历史 Hash、规则测试和原提示词审计。

- 仅保留 **自定义模型审计**。审计地址、模型、节点 API Key、超时、提示词、风险阈值及连接测试均在审计池管理，不再有独立“审计凭证”页或审核引擎切换。旧 `/admin/risk-control/keys` 页面跳转到 `/admin/risk-control/model-audit`。
- 删除 **邮件通知**。SMTP 发送、通知队列和凭证设置停止使用；旧字段接受但忽略。
- 删除 **适用范围**。审核统一适用于全部模型、分组和调用方 Key，旧 include/exclude 及 ID 范围不再跳过审核。审核模式、采样率及仅关键词策略继续生效；这不影响上游路由本身的模型、分组和配额限制。
- 删除 **调用方 Key 封禁**。没有自动封禁、违规计数窗口或解封接口，旧封禁行仅作历史保留，不参与鉴权。

## 审核未通过直接走兜底池

位置：**风控中心 → 审核策略 → 审核命中与兜底**。

配置字段 `fallback_on_block_enabled` 默认 `true`；新安装与缺少该字段的旧配置都默认开启。显式关闭状态在保存、部分更新和重启后保留。

- 开启：同步审核命中关键词、Hash 或自定义模型阻断阈值后，直接进入已启用且匹配的兜底池，不先调用主池。兜底仅执行一次终态尝试，失败不返回主池。
- 关闭：按配置的审核状态码与文案直接拦截，主池和兜底池均不调用。
- 未启用兜底池或没有匹配账号：返回审核拦截错误，不回主池。此开关不代替兜底池本身的启用开关。
- 覆盖 HTTP Responses、Compact、Chat Completions、Messages，以及自包含的 Responses WebSocket 轮次；WS 每轮重置路由状态。
- 已绑定上游的 `previous_response_id`、压缩来源和 turn-state 续链不跨供应商迁移。媒体/Realtime 无对应兜底执行器时仍拦截；原生 Claude 账号选定后的正文二次审核仍拦截，避免误投主池。
- 原提示词过滤独立执行。观察模式、审核服务故障以及仅标记结果不强制进入兜底池；观察模式历史 Hash 命中保留拦截语义。
- 审核日志保留原始判定，`blocked` 表示审核未通过；请求日志记录实际路由与结果，兜底原因是 `content_review`。

## 自定义模型审计池

1. 新增节点，填写名称、OpenAI 兼容 Base URL（根地址或 `/v1` 结尾）、自定义模型名称、API Key、超时与单片上限。模型名称不受主池模型目录限制。
2. 先“应用到草稿”，再点击页头“保存配置”。普通 HTTP 页面也支持新增节点，不依赖仅安全上下文可用的 `crypto.randomUUID()`。
3. 多节点按顺序尝试；连接、HTTP 或解析失败时换下一节点。节点超时 100–120000 ms，审核总预算最多 120 秒。空节点池或全部失败自动放行，继续正常上游路由，不返回审核 503、不触发内容审核兜底。旧 `fail_open=false` 在加载和保存时统一归一化为 `true`；界面不再提供失败阻断开关。审核故障始终记录错误原因，“保存审核通过记录”仅控制正常通过事件。
4. 在“审核策略”页统一设置审核开关、审核模式及关键词策略；自定义模型审计页只显示配置草稿摘要，并提供跳转入口。启用内容审核，选择“关键词 + 模型审计”或“仅模型审计”。仅关键词策略跳过模型调用。
5. 共享 system 提示词支持自定义与九类分类模板。提示词区的“保存审核提示词”与页头“保存配置”使用同一保存流程，保存当前全部配置草稿，保存成功后生效；试审不等于保存。后端调用 `/v1/chat/completions`，用户文本转义后放入 `<user_input>`，不运行模型工具。
6. 连接测试与试审使用当前草稿，不必先保存；消耗所选模型额度但不写业务日志、Hash，也不调用兜底池。

模型返回示例：

```json
{"risk":"unsafe","confidence":0.95,"categories":["violence"],"reason":"简短原因"}
```

只提取当前用户文本，规范化后最多 400,000 字符，按启用节点的最小单片上限分片；不审核生成后的内容。safe 放行，较低风险只标记，unsafe 达阻断阈值后由兜底开关决定路由。管理员接口省略完整节点密钥，编辑留空保留，显式勾选才清除。

## 旧配置兼容与部署

- 新旧配置统一为自定义审计池，已有自定义节点、密钥、提示词与阈值保留。
- 旧 Moderations 凭证不自动转换成 Chat 节点（协议不同）。之前只配置了旧审计凭证的部署，应先新增并测试自定义节点；否则按审计池故障自动放行并记录错误。
- 旧范围、邮件及封禁字段仍可解析，但加载/保存时归一化，不保留隐藏限制或后台通知。现有审计日志与 Hash 不删除。
- 先备份数据库，再构建前端和 Go 程序并部署重启。多副本需统一更新，配置与观察队列的运行状态不跨实例聚合。
- 回退到带封禁和范围功能的旧版本前需评估历史封禁行；旧程序可能再次使用这些历史数据。

## 回归入口

```powershell
go test ./security/riskcontrol ./database ./admin ./proxy -run TestRiskControl -count=1
go test ./... -count=1 -timeout 8m
go vet ./...
cd frontend
npm test
npm run typecheck
npm run test:risk
npm run build
```

浏览器回归使用真实普通 HTTP 页面组件，验证新增/保存自定义模型、兜底开关默认开启及关/开后刷新保留，并检查退休设置不再出现。数据库测试验证旧配置全模型生效、旧通知停用及开关重启保留。

## 历史验证记录（2026-09-19）

以下为当日原版本的验收记录，封禁行为不代表当前版本。

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
