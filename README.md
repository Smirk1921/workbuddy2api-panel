<p align="center">
  <img src="https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/WorkBuddy.png" alt="WorkBuddy2API" width="120">
</p>

<h1 align="center">WorkBuddy2API Panel · Smirk1921 复刻</h1>

<p align="center">
  <b>把腾讯 CodeBuddy 账号变成 OpenAI 兼容 API 的多账号网关 · 附 Web 管理面板</b><br>
  本仓库是上游的个人复刻（fork）：新增「低积分自动冻结 / 指定账号优先 / 账号分组批量操作」并修复面板窄窗口布局，其余与上游保持一致。
</p>

---

> **本仓库是 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) 的 GitHub Fork（复刻）。**
> 上游是持续维护的增强分支；本复刻基于其 `15a6fc9`（v1.11.11-panel 之后的最新上游），
> 之上仅叠加五个提交——差异见 [本复刻相对上游的差异](#本复刻相对上游的差异)，其余行为与上游完全一致。

> ⚠️ **同样适用上游的使用声明**：本项目仅限自用账号（签到 / 保活 / 个人工具接入），
> 不支持也禁止批量小号分发额度、二次打包或收费售卖。详见 [使用声明](#使用声明)。

## 复刻链路

```
Sliverkiss/workbuddy2api                    原始项目：CodeBuddy → OpenAI 兼容网关
        │                                   （原仓库已被作者删除）
        ▼  增强分支（独立仓库，非 GitHub fork 关系）
linguo2625469/workbuddy2api-panel           上游：Web 面板 / 账号池 / 任务自动化
        │                                   持续维护，本复刻的同步来源
        ▼  GitHub Fork
Smirk1921/workbuddy2api-panel               本仓库：复刻（个人自用增量）
```

| 角色 | 仓库 | 说明 |
|------|------|------|
| 原始项目 | `Sliverkiss/workbuddy2api` | 网关原型；GitHub 仓库已被作者删除 |
| 上游 | [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) | 在原始项目基础上重建的增强分支，独立维护 |
| **本复刻** | [Smirk1921/workbuddy2api-panel](https://github.com/Smirk1921/workbuddy2api-panel) | 上游的 Fork，叠加下述自用改动 |

**当前基线**：上游 `6b31db6d`（v1.13.0-panel 之后，含到期卡片死锁修复、tool_call 脏数据剔除、prompt_cache_key 回退等 9 个提交）。

## 本复刻相对上游的差异

仅五个提交，全部叠在上游历史之上（rebase 维护，随时可跟上上游）：

### 1. 低积分自动冻结 —— `bbc95c0`

每个账号可单独设置「冻结阈值」（账号行「阈值」按钮，`0` = 关闭）：

- 剩余积分 **低于阈值** → 自动冻结，不再参与选号（面板显示「低积分冻结」标签，总览有对应统计卡片）；
- 余额 **回到阈值以上** → 自动解冻（余额刷新 / 签到时判定，无需人工干预）；
- 与「禁用」正交：禁用是人工 / 故障终态，冻结是低积分保护，可自动恢复；
- 与上游的「积分保底 credit_floor」互补共存——保底按「模型是否收费 × 余额」动态拦截，
  冻结按「用户设定阈值」静态保护，二者在选号路径同时生效；
- 实现覆盖：账号池状态机（`freezeLocked` / `unfreezeLocked` 原语、余额刷新与请求扣费双检查点、
  `state.json` 持久化与脏数据对账）、`POST /panel/api/accounts/{uid}/freeze_threshold` 端点、
  面板 UI、单元测试。

**用法**：面板 →「账号池」→ 目标账号行 →「阈值」→ 输入数值（如 `200`，`0` 关闭）。

### 2. 指定账号优先使用其积分 —— `2299ab3`

每个账号可开启「优先」开关（账号行「优先」按钮，开启后高亮）：

- **只要该号可用**（健康、未触积分保底、在途未满），选号就只在优先号中进行——先用完它的积分；
- 该号不可用时**自动回落普通池**（不会出现无号可用）；
- 优先级**高于成本分层**（人工显式意图 > 自动的「免费模型优先」），否则优先号一旦实测收费就会被
  成本分层整层滤掉、开关等于失效；
- 与冻结 / 禁用正交（偏好维度 ≠ 可用性维度）；禁用 / 冻结 / 积分保底 / 在途上限等安全机制
  **不被优先绕过**；
- 新会话分配给优先号更高的虚拟实例权重（明显偏置但不独占，避免把单号在途打满）；
- 多个账号同时设优先时，优先层内部仍按既有权重（积分 / 闲置 / 快过期）加权轮换。

**用法**：面板 →「账号池」→ 目标账号行 →「优先」（再点一次取消）。
与「阈值」配合即「先用这个号的积分，用到低于阈值自动停」。

### 3. 账号分组与按组批量操作 —— `e33ec29`

给账号打「分组」标签（账号行「分组」按钮，空 = 移出分组），支持按组筛选与批量操作：

- 账号池顶部新增**分组筛选下拉**（全部 / 未分组 / 各分组）+ 作用域计数；
- 选中某分组后，右侧批量按钮作用于该作用域：**优先 / 阈值 / 冻结 / 解冻 / 移除**；
- 组标签是**纯元数据**——不影响选号 / 冻结 / 禁用 / 优先等任何池内行为；
- 组名持久化在 `state.json`（不在 `auths/*.json`，重新登录不会丢）；
- **「移除」是不可逆的**：会要求逐字输入组名确认，并删除凭证文件（恢复需重新登录）。
- 接口：`POST /panel/api/accounts/{uid}/group`、`GET /panel/api/groups`、
  `POST /panel/api/groups/{group}/{action}`。

**用法**：面板 →「账号池」→ 顶部「分组」下拉选一组 → 右侧批量按钮；
或用行内「分组」按钮逐个归组。

### 4. 面板窄窗口右侧显示不全修复 —— `9e7c299`

行内按钮增至 8 个（签到 / 余额 / 任务 / 阈值 / 优先 / 分组 / 解冻|禁用 / 移除）后，
账号表在窄窗口曾把最右侧「禁用 / 移除」裁切。修复后：

- 宽屏（>1470px）：动作列一行 8 个按钮；
- ≤1470px：动作列固定宽 + 按钮换行，全部按钮可见；
- ≤1400px：用量列隐藏 tok/s 指标、账号列按窗口收窄，为按钮让出横向空间。

实测 1200~1920px 十二档宽度：按钮零裁切、表格零横向溢出。

### 5. 积分到期提醒卡片可折叠 —— `73d25dc`

上游的「积分到期提醒」卡片默认展开且每账号一行（30 账号 ≈ 1200px），进入账号池视图即把
「账号池」表格挤到屏幕外。改为**默认收起**（只留标题一行 + 摘要：最近到期日 ·
30 天内到期账号数 · 查询失败数），展开态**封顶 44vh + 内部滚动**，展开/收起状态
持久化到 localStorage；点「检查」自动展开。

### 6. 账号导入修复：只有 access token 的凭证也能导入 —— 已向upstream提issue

`POST /panel/api/import/cockpit` 原先硬性要求 `uid + access_token + refresh_token` 三件套，
而 cockpit tools 对「只拿到 access token」的账号会导出 `refresh_token=""`、`expires_at=0`。
这类记录与「字段全缺」合并进同一个跳过分支，导入结果是**成功 0 个、跳过 N 个**，且原因只写进
`console.warn`——面板上一个字都不显示，用户只能看到"导入完成"却一个号都没进来。

改动（`internal/panel/import.go` + `internal/panel/app.js` + `internal/server/handler.go`）：

- **refresh_token 改为可选**。access token 自带 JWT `exp`，账号在到期前完全可用，没有理由拒绝；
  过期时间按 `expires_at`（毫秒/秒自适应，旧实现无条件 ÷1000 会把「秒」除成 1970）→
  JWT `exp` 依次回退；`uid` 缺失时回退 JWT 的 `sub`。
- **跳过原因与警告上屏**。响应新增 `warnings`；前端新增明细区（✗ 跳过原因 / ! 警告），
  「一个都没进来」按失败样式显示而不是"导入完成"。
- **文件形态兼容**：顶层单对象、`{"accounts":[...]}` 信封、以及带 UTF-8 BOM 的文件
  （Go 的 `encoding/json` 不吞 BOM，PowerShell 5.1 的 `Set-Content -Encoding utf8` 会写 BOM）。
- **凭证不可续期时明确禁用**：`handler` 在需要刷新但 `refresh_token` 为空时，按
  `missing refresh_token (re-login required)` 禁用并返回 `credential_not_renewable`，
  而不是每次请求都在刷新失败上白换一轮号、堆熔断计数。
- **一次可选多个文件**：面板「导入 JSON」支持多选（可一次拖入整个账号目录）。逐文件
  **顺序**上传（每个号导入时都会顺带打一次上游，并发上传容易撞风控），完成后汇总
  「成功 / 跳过 / 几个文件处理失败」；每条原因都带文件名前缀，坏文件只跳过自己、
  不影响其余文件。

### 7. 成长任务周期清扫（补领卡在 4000 以下的积分）

新号跑完成长任务，进度其实已经上报生效（服务端 progress 达标），但积分停在
**2000/2100/2300 上不去 4000**——差的正是「领奖」这一步。上游只有
`schedule.growth_hours`（默认 01:00）的成长队列，而它只处理「被扫进队列的任务」，
两类场景覆盖不到：

- 你在代理里**真实对话点亮的任务**——服务端进度自己涨，面板并不知情，没人去 claim；
- 历史上**漏领的达标任务**——任务早已完成，只是没人领。

新增 `schedule.task_sweep_enabled`（默认 true）+ `schedule.task_sweep_minutes`
（默认 15）的独立周期作业（`internal/panel/tasksweep.go` + scheduler 的
`StartTaskSweep`，与 `balance_refresh` 同款分钟级 ticker，支持在线改配置即时生效）：

1. accept 所有 `not_accepted` 的待办任务（默认口径与小程序口径分别提交）；
2. claim 所有「进度达标且未领取」的任务（mp 码走 mp 口径，其余走 Web 口径）。

**全幂等**（accept 幂等；claim 重复领返回 `already_claimed` 不计新增），**不跑任何行为
链路**（那是「成长任务自动执行」的职责），所以开销很小：每账号 2 次列表 + 少量领奖请求。
领奖判据只用 progress（`Claimable`）——`accept_status` 会短暂转 `completed` 再回滚，
拿它当依据会误领。global 与企业版账号不发任何上游请求。

### 8. 账号批量操作升级：表格多选 + 动作对齐

第 3 项的分组批量只能按「组」作用域操作，且动作不全（没有暂停/恢复选号、不能移组），
组批量里那个「冻结」按钮实际调的是 `Disable`（真禁用，需手动恢复），与账号状态里的
低积分 `frozen` 是两回事。本版补齐：

- **表格多选**：账号池每行一个勾选框（表头全选只作用于当前筛选后的可见行），
  勾选集合跨 5 秒轮询存活；**有勾选时批量动作只作用于选中，无勾选时作用于分组作用域**。
  勾选集合随视图收敛（只保留可见账号）——切分组/切状态筛选、或批量移组把账号移出视图后
  不会残留「幽灵选中」被带进下一次批量。
- **状态筛选 + 选中 11140**：状态下拉（全部 / 11140 风控封禁 / 可用 / 冷却中 / 低积分冻结 /
  暂停选号 / 已禁用）与分组筛选叠加；一键「选中 11140」直接筛出被上游 11140 风控封禁的
  账号并全选（`disabled_reason` 里带 11140 的），便于一次性禁用 / 移除 / 移组。
- **动作与账号行按钮 1:1**：暂停选号 / 恢复选号 / 禁用 / 恢复 / 阈值 / 优先 / 移组 / 移除。
  组批量的按钮文案改为「禁用 / 恢复」，tooltip 说清与「低积分冻结」的区别。
- **统一执行器**：新端点 `POST /panel/api/accounts/batch`（uid 列表）与组作用域端点共用
  同一份实现（`internal/panel/batch.go`）；`freeze` 保留为 `disable` 的兼容别名。
- **可观测**：响应区分 `affected`（命中，含幂等 no-op）与 `changed`（状态真的变了），
  并返回逐号 `results[]`；前端用明细区列出跳过/失败原因（锁占用、账号不存在等）。
- **逐号互斥**：批量动作对每个账号做非阻塞试锁，正在跑任务链路的账号本轮跳过。
- 修掉两个既有缺陷：①「未分组」的批量移除永远 400（作用域被归一化成空串后再比对
  confirm）；② `SetGroup` 按 64 **字节**截断会把汉字切成半个（落盘后是非法 UTF-8），
  改为按字符截断并拒绝 `__all__` / `__ungrouped__` 保留字。

### 9. 任务节奏可配置：动作之间 / 账号之间

自动做任务的风控节流间隔原先硬编码。新增两个配置（duration 字符串或纯数字按秒，
空 = 默认，非法快速失败，上限 5 分钟）：

| 键 | 默认 | 作用 |
|---|---|---|
| `schedule.task_action_gap` | `1050ms` | 每个动作 / 上报之间（上报、批量 accept、领养前置等待） |
| `schedule.task_account_gap` | `200ms` | 每个账号之间（任务清扫 / 旅行 / 活跃上报 / 夜猫子） |

默认值与引入前逐字一致，零行为变化；支持在线改（面板保存后下一轮生效）。
**刻意不纳入**：小程序对话事件间隔（45s，反作弊硬约束）、领奖回读轮询预算、
专家召唤间隔（实测校准）——它们不是「节奏」，调小会直接坏功能。

### 10. 仪表盘 + 积分口径开关

新增「仪表盘」视图与聚合接口 `GET /panel/api/dashboard?hours=168`（全部由内存状态 +
本地用量记录器聚合，**零上游请求**）：

- 10 张 KPI 卡（三档积分口径 / 账号健康度 / cn·global 分域 / 30 天内到期）；
- Token 时序图（复用用量页图表）、分组概览、积分榜 Top8、模型锁池；
- **积分口径开关**：默认「排除低积分冻结 + 暂停选号」，勾上后只算「可服务」账号
  （再排除禁用与冷却中）；账号池那张「积分剩余 / 总额」读同一开关，两端数字一致，
  选择存 localStorage。
- 顺手修掉一个长期口径错误：企业版 `credits_total = -1`（不限量）此前被直接累加进
  总额（总额少 1），现在单独计数。

### 11. 账号导出（面板内置，JSON，不打包）

面板内置 `GET /panel/api/export/accounts?uids=…&credit=credits|credits_total&mode=combined|split`，
账号池工具栏的「**导出**」按钮即调它（有勾选导勾选，无勾选导当前可见集）。
导出方式由「导出」左侧的下拉框选择，选择存 localStorage。

**服务端只产出 JSON，不生成 zip；需要按组目录/压缩时交给本地打包脚本。**

| 方式 | 导出范围 | 文件名 | 内容 |
|---|---|---|---|
| `combined`（默认） | 1 个账号 | **`<账号名>-<积分>.json`** | 与「账号」目录里的既有文件（`60800953-4000.json`）**逐字段一致**的单元素数组 |
| `combined`（默认） | 多个账号 | `账号导出-<时间戳>.json` | 账号数组；每条在规范 10 字段之外附 `group` 与 `credits`（导入端忽略未知字段） |
| `split` | 任意数量 | 每个账号 **`<账号名>-<积分>.json`** | 服务端返回文件清单（`files[]`：`name` / `group` / `credits` / `content`），前端逐账号触发下载；不生成 zip |

`split` 的清单响应不设置 `Content-Disposition`，避免浏览器把清单本身当文件下载；
`content` 只含规范 10 字段，`group`/`credits` 放在清单项上。浏览器可能询问是否允许多文件下载。

- **昵称还原**：实例落盘的昵称是打码形态（`703****0`，84 个号里 51 个是这种），
  导出时用 access_token 的 JWT `preferred_username` 还原，**并与打码值交叉校验**
  （前 3 位 + 尾部长度），对不上就保留打码值并在 `X-Export-Warnings` 里计数，绝不猜；
- 缺 `access_token` 的账号跳过并计入 `X-Export-Skipped`；响应带 `Cache-Control: no-store`，
  端点受面板 api_key 保护。

**需要按组目录 / zip 时**用同目录的打包脚本（`打包导出JSON.ps1`）：

```powershell
pwsh -NoProfile -File .\打包导出JSON.ps1 -Json <导出的.json> -Zip
```

它把导出文件拆成 `主力/98175153-3096.json`、`备机/…`、`未分组/…`，剔除 `group`/`credits`
还原成规范格式，`-Zip` 时再打成 `打包-<时间戳>.zip`（并校验条目数）。

### 12. 429 code 14003（模型繁忙）只停模型，不整号冷却

上游 `429 + code 14003` 的完整文案是 `too many requests` / `模型繁忙，请换模型或稍后重试`，
body 里**没有**恢复时刻。它与 `6004` 一样是**模型级**信号：同号换模型立即可用，
但网关此前只把 `6004` 认成模型级，14003 掉进账号级软冷却（`soft_rate` 600s 起、
指数退避到 2h）。线上形态是一个请求轮转几个号，每个号都中 14003，几十秒内整池
对同一模型冷却 10 分钟。

本复刻移植了上游 PR #234 / 另一复刻 PR #37 的修复（两个 PR 都被关闭，未进上游主线）：

- `6004` 与 `14003` 都识别为模型级（`IsModelRateLimit`）；
- `14003` 且无 `Retry-After` → `CooldownModelBusy`：只停 (账号, 模型) **60s 起、
  按命中翻倍、封顶 3 分钟**；窗口内不延长，已有更晚冷却不缩短；
- `6004` 带「将在 … 重置」→ 对齐上游墙钟；无重置时间 → 也只停 (账号, 模型)，
  时长取 `soft_rate` 基数并按命中次数退避，**不再整号冷却**；
- 其余 429 仍走账号级 `soft_rate` 阶梯；
- 账号级 `until` 与 `soft_streak` 在模型级限流时全程不动，切模型即可用。

涉及文件：`internal/upstream/client.go`、`internal/pool/cooldown.go`、
`internal/server/handler.go`；新增测试覆盖 14003/6004 无重置时间、短窗口封顶、
不缩短已有更晚冷却、换模型仍可选。

### 13. 四个专家体验任务（`expert_use_*`，截止 2026-11-20）

2026-10-23 服务端新增四个「在专家页搜索并召唤「X」，完成一次对话」任务，每项
+50 积分 +5 能量，均截止 **2026-11-20**：

- `expert_use_maidehao`：中国电商运营专家（卖得好，`ex_GDl31WahyIH0`）
- `expert_use_douyinyin`：抖音策略师（斗音音，`ex_FbY5nlWTqDod`）
- `expert_use_shangjia_xuanchuan`：商家宣传页与海报设计师（`ex_Weo3x2EZuSDm`）
- `expert_use_bowangwang`：直播电商教练（播旺旺，`ex_WZY136wV5aMt`）

判据链与 `Expert_lighthouse` 同源：专家市场按 **keyword** 搜索 → **按 `expert_id`
精确落位**（同名专家很多，不能取首条）→ 召唤事件 → 真实对话取服务端 `requestId`
→ `expert_actual_use(mode=LOCAL)`。昵称写 `name`、职业名写 `expertTitle`，不能互换。

实现参考活跃复刻 `misakano7545/workbuddy2api-panel` 的 `4c45b1d`，按本分支结构适配：
`internal/panel/autotask.go` 新增 `expertExperienceTasks` / `runExpertExperience`，
`internal/upstream/desktop.go` 新增 `MarketExpertSearch`，前端 `AUTO_TASKS` 同步登记；
新增 `expert_experience_test.go` 覆盖「同名专家按 id 落位」「搜索失败静态兜底」
「未登记任务码报错」三类回归。

### 14. 429 code 6021（上游队列已满）不罚号、不轮转

上游对模型请求队列打满时返回 `429 {"code":6021,"msg":"queue.full.title"}`
（`queue.full.title` 是未翻译的 i18n key）。它与 6004/14003 的区别是**不区分模型**：
实测另一台实例上 11 个不同账号在 14 秒内全部命中，每个账号当时只有 1 条在途请求——
这是服务级甩负载，不是账号级限流。

网关此前把它归进通用 429（`ErrSoftRate`），于是每个中招账号被账号级 `soft_rate`
罚停 600s（指数退避封顶 2h）；`MaxRotate=3`，4 次客户端请求就冷掉 11 个号，
只能人工 revive。

本复刻的修复：

- 新增 `ErrQueueFull`：识别业务码 `6021` 与固定文案 `queue.full`（`IsQueueFull`），
  判定先于通用 429 兜底；
- 轮转侧 **fail-fast**：首个 6021 即终止换号（换号打的是同一个队列），
  一次客户端请求最多 1 次上游调用；
- 账号零惩罚：不冷却、不熔断、不喂连败；
- 末端回 `429` + `code=upstream_queue_full` + `Retry-After: 5`（上游带
  Retry-After 头时以其为准），上游原文（含 requestId）透传；
- SSE error 帧形态由 `FrameKind` 同步识别。

涉及文件：`internal/upstream/client.go`、`internal/upstream/hint.go`、
`internal/server/handler.go`；新增测试覆盖 6021/文案识别、数字串不误命中、
fail-fast 单次上游调用、账号不冷却、Retry-After 与原文透传。

### 附：摘取的上游补丁与调研结论

- **PR #217（同账号 refresh 单飞）已摘入**：上游对 refreshToken 有反重放，同一 RT 被
  并发消费两次会返回 12153 并作废整条 RT 链。现按 UID 单飞（follower 复用 leader 结果），
  并对 12153 读磁盘 auth 文件、若已被别的进程 rotate 则只重试一次。
- **国际版（global）签到：调研结论为「不需要实现」**。用两个真实 global 账号实测：
  `daily-checkin` 端点存在但返回 `HTTP 400 / code 10001「签到活动未开启或已过期」`；
  每日 `/v2/report` 上报成功但余额不变；两个号的**连登都是 4 天**——说明「登录即签到」
  在国际版体现为连登，而调度器本来就不跳过 global 的每日活跃上报，语义已等效覆盖。
  国际版的积分增益只有一次性 trial。复现工具：`go run ./cmd/checkinprobe -dir ./auths`。

**完整差异对照**：[`linguo2625469:main...Smirk1921:main`](https://github.com/Smirk1921/workbuddy2api-panel/compare/linguo2625469:main...Smirk1921:main)

## 与上游同步

本复刻用 rebase 保持「上述提交叠在上游之上」的线性结构：

```bash
git remote add upstream https://github.com/linguo2625469/workbuddy2api-panel.git  # 仅首次
git fetch upstream main
git rebase upstream/main    # 本复刻的提交重放到上游最新之上
git push --force-with-lease origin main   # rebase 重写了 SHA，普通 push 会被拒
```

> **README.md 冲突的处理（重要）**：本 README 是复刻自有文档，同步上游时若冲突应保留本文件。
> 注意 **rebase 下 `--ours/--theirs` 与 merge 相反**——`--ours` 指「被重放到的上游基线」，
> 我们的提交才是 `--theirs`。正确做法：`git checkout --theirs README.md && git add README.md`
> （用 merge 的直觉写 `--ours` 会把本文件覆盖成上游 README）。

## 快速开始

完整文档（部署方式 / config 全字段 / API 用法 / 面板说明 / 成长任务）见上游 README：
👉 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel#readme)

**源码构建（Go 1.22+）：**

```bash
go build -trimpath -ldflags="-s -w" -o wb2api.exe ./cmd/server
./wb2api.exe -config config.json      # 首次运行自动生成推荐配置（含随机 api_key）
```

**Docker：**

```bash
docker compose up -d --build
```

浏览器打开 `http://127.0.0.1:7863/panel/`，用「添加账号」登录 CodeBuddy 账号即可。

## 使用声明

与上游一致：本复刻**仅限自用账号**（自动签到 / 保活 / 给自己的工具接入 API），
**不授权、不支持**批量注册小号分发额度、账号池出租、二次打包售卖或任何形式的收费分发
——此类行为违反目标平台服务条款，且与作者无关；请勿购买任何「收费版 / 卡密版」。

完整声明（含风险提示与作者立场）见上游 README 的「使用声明（必读）」章节：
👉 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel#readme)

## 许可

[MIT](LICENSE)，继承自上游：原始项目 © 2026 Sliverkiss，上游 © 2026 linguo2625469。

---

<sub>本 README 为复刻自有文档；上游完整文档以上游仓库为准。同步上游时如遇 README.md 冲突，保留本文件（rebase 下用 `--theirs`）。</sub>
