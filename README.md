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

**当前基线**：上游 `d66384d9`（**v1.13.0-panel**，含企业版能力门控、模型锁池视图、暂停选号单列统计等 17 个提交）。

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

**完整差异对照**：[`linguo2625469:main...Smirk1921:main`](https://github.com/Smirk1921/workbuddy2api-panel/compare/linguo2625469:main...Smirk1921:main)

## 与上游同步

本复刻用 rebase 保持「五个提交叠在上游之上」的线性结构：

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
