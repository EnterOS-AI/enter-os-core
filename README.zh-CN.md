<div align="center">

<p>
  <img src="./docs/assets/branding/enteros-logo.svg" alt="EnterOS" width="420" />
</p>

<p>
  <a href="./README.md">English</a> | <a href="./README.zh-CN.md">中文</a>
</p>

<h3>AI Agent 团队的操作系统</h3>

<p>
  EnterOS 是组织原生的控制平面，把异构 AI agents 作为一个受治理的组织运行起来：统一运行时契约、层级对齐的记忆，以及真正面向生产的控制平面。
</p>

<p>
  <a href="https://www.enteros.ai"><img alt="官网" src="https://img.shields.io/static/v1?label=&message=Website&color=5B5BF7&style=for-the-badge"></a>
  <a href="#快速开始"><img alt="快速开始" src="https://img.shields.io/static/v1?label=&message=Quick%20Start&color=111827&style=for-the-badge"></a>
  <a href="./docs/architecture/architecture.md"><img alt="系统架构" src="https://img.shields.io/static/v1?label=&message=Architecture&color=374151&style=for-the-badge"></a>
</p>

[![License: BSL 1.1](https://img.shields.io/badge/License-BSL%201.1-orange.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.25+-00ADD8?logo=go)](https://golang.org/)
[![Python Version](https://img.shields.io/badge/python-3.11+-3776AB?logo=python)](https://www.python.org/)
[![Next.js](https://img.shields.io/badge/Next.js-15-black?logo=next.js)](https://nextjs.org/)

<p>
  角色原生 Workspace • Memory v2 (pgvector) • Claude Code · Codex · Hermes · OpenClaw
</p>

<p>
  <a href="./docs/index.md"><strong>文档首页</strong></a> •
  <a href="./docs/quickstart.md"><strong>快速开始</strong></a> •
  <a href="./docs/architecture/architecture.md"><strong>系统架构</strong></a> •
  <a href="./docs/api-protocol/platform-api.md"><strong>Platform API</strong></a> •
  <a href="./docs/agent-runtime/workspace-runtime.md"><strong>Workspace Runtime</strong></a>
</p>

[![Deploy on Railway](https://railway.app/button.svg)](https://railway.app/new/template?template=https://github.com/EnterOS-AI/enter-os-core.git)
[![Deploy to Render](https://render.com/images/deploy-to-render-button.svg)](https://render.com/deploy?repo=https://github.com/EnterOS-AI/enter-os-core.git)

</div>

---

## 为什么团队选择 EnterOS

EnterOS 为 AI 平台团队补上 agent runtime 与生产环境之间缺失的操作层。它把 agent 建模为持久角色，让组织图直接成为协作拓扑，把记忆限定在真实团队边界内，并为生命周期、审批、追踪、文件、终端和 runtime 控制提供实时运维界面。

| 你需要什么 | EnterOS 如何解决 |
|---|---|
| **可治理的 Agent 团队** | 用角色、层级、生命周期、secrets、approvals 和 activity history 管理长期运行的 workspaces。 |
| **运行时自由度** | 让 Claude Code、Codex、Hermes、OpenClaw 共用同一 workspace contract，而不是把所有团队锁进同一个 framework。 |
| **可安全复利的记忆** | 通过 `LOCAL`、`TEAM`、`GLOBAL` 分层作用域，以及 Memory v2 + pgvector 语义召回，让记忆沿组织边界沉淀。 |
| **生产级运维面** | 通过 Canvas、WebSocket、Langfuse traces、terminal、files 和 runtime tiers，检查、暂停、重启、观测并持续改进 live workspaces。 |

## 一句话定位

EnterOS 是 AI Agent 团队的操作系统：一个组织原生的控制平面，用统一运行时契约、层级对齐的记忆和生产级运维能力，把异构 agents 作为一个受治理的组织运行起来。

它把过去分散在 demo、内部胶水代码和各类 framework 私有工具里的关键能力，收敛成一个产品：

- 一套组织原生 operating layer，管理团队、角色、层级、权限和生命周期
- 一套 runtime abstraction，让 **4 个**维护中的 agent runtime —— Claude Code、Codex、**Hermes**、OpenClaw —— 共用一套 workspace 契约
- 一套与组织边界对齐的 memory 模型，把 recall、sharing 和 skill evolution 放进同一体系（Memory v2 由 pgvector 支撑语义召回）
- 一套面向线上 workspace 的运维面，统一完成观测、暂停、重启、检查和持续改进

今天很多团队能做好 workflow、单 agent、coding agent，或者自定义 multi-agent graph 中的一种。

但很少有团队能把这些能力一起运行成一个有清晰结构、稳定 memory 边界和生产级运维能力的 Agent 组织。

EnterOS 填的就是这个空白。

## 为什么 EnterOS 很不一样

### 1. 节点是角色，不是任务

在 EnterOS 里，workspace 是一个组织角色。这个角色今天可以是单 agent，明天可以扩成内部子团队，而且对外身份、层级位置、memory 边界和 A2A 接口都不变。

### 2. 组织图就是拓扑

你不需要手动画协作边。层级天然决定默认协作路径。这里的组织结构不是装饰性 UI，而是运行模型本身的一部分。

### 3. Runtime 选择不再是死路

Claude Code、Codex、Hermes、OpenClaw 都可以挂到同一个 workspace abstraction 下。团队可以统一治理方式，而不必统一到底层 runtime。

### 4. Memory 被当成基础设施来做

EnterOS 的 HMA 不是“多存一点上下文”而已。它关注组织边界、durable recall、scope sharing、v2 memory plugin、skill promotion，把这些放在一个完整体系里。

### 5. 它自带真正的 control plane

Registry、heartbeat、restart、pause/resume、activity、approval、terminal、files、traces、bundles、templates、WebSocket fanout 都不是补丁，而是平台一等能力。

## EnterOS 填补了什么市场空白

| 类别 | 擅长什么 | 通常卡在哪里 | EnterOS 补上的部分 |
|---|---|---|---|
| Workflow builder | 可视化任务编排 | 节点是任务，不是持久组织角色 | 角色原生 workspace、层级结构、长期团队 |
| Agent framework | Runtime 语义强 | 缺统一 control plane 和组织级运维 | 生命周期、Canvas、registry、策略、observability |
| Coding agent | 本地执行很强 | 不适合直接当团队基础设施 | Workspace abstraction、A2A 协作、平台化运维 |
| 自定义 multi-agent graph | 灵活度高 | 拓扑脆弱、治理分散 | 在保留 runtime 自由度的同时统一 operating model |

## EnterOS 的可防御优势

| 优势 | 为什么重要 |
|---|---|
| **角色原生 workspace 抽象** | 模型切换、框架切换、团队扩容都不会打碎你的组织结构 |
| **分形式团队扩展** | 一个 specialist 可以平滑升级成一个部门，而不影响上游集成 |
| **异构 runtime 兼容** | 不同团队可以保留偏好的 agent 架构，但共用一套平台规则 |
| **HMA + v2 memory plugin** | Memory 分享沿组织边界走，而不是全局乱穿透；每个 tenant 一个 plugin，按 workspace namespace 隔离 |
| **Skill 演化闭环** | 成功工作流可以从 memory 逐步提升成可热加载的 skill |
| **WebSocket-first 运维体验** | Canvas 能即时反映任务状态、结构变更和 A2A 响应 |
| **Global secrets + local override** | 统一管理 provider 凭据，只在需要时做 workspace 级覆写 |

## 兼容哪些 Agent 架构，怎么对比

EnterOS 并不是要替代下面这些 framework，而是把它们纳入更强的组织级 operating model。

| Runtime / 架构 | 当前仓库状态 | 原生优势 | EnterOS 额外补上的能力 |
|---|---|---|---|
| **Claude Code** | `main` 已支持 | 真实编码工作流、CLI-native continuity | 安全 workspace 抽象、A2A delegation、组织边界、共享 control plane |
| **Codex** | `main` 已支持 | OpenAI Codex CLI 工作流 | 安全 workspace 抽象、A2A delegation、组织边界、共享 control plane |
| **Hermes 4** | `main` 已支持 | 混合推理、原生工具调用、json_schema 输出（NousResearch/hermes-agent） | Option B 上游 hook、A2A 桥接 OpenAI 兼容 API、多 provider 自动派生 |
| **OpenClaw** | `main` 已支持 | CLI-native runtime，自有 session 模型 | workspace 生命周期、templates、activity logs、拓扑感知协作 |
| **NemoClaw** | `feat/nemoclaw-t4-docker` 分支 WIP | NVIDIA 方向 runtime 路线 | 计划并入同一抽象层，但当前还不是 `main` 已合并能力 |

核心价值就是：**多种 agent runtime，共用一套组织级操作系统**。

## 为什么我们的 Memory 架构会越跑越强

很多项目停留在“加了 memory”。EnterOS 走得更远：

| 常见 memory 方案 | EnterOS |
|---|---|
| 扁平 store 或弱命名空间隔离 | 与层级对齐的 `LOCAL`、`TEAM`、`GLOBAL` scope |
| 分享很容易越界 | 分享是显式且结构感知的 |
| Memory 和 procedure 混成一团 | Memory 存 durable facts，skills 存 repeatable procedure |
| 任意 agent 容易过权 | v2 memory plugin 的 per-workspace namespace 缩小 blast radius |
| UI memory 和 runtime memory 混在一起 | scoped agent memory、key/value workspace memory、recall surface 分层清晰 |

### 这套飞轮怎么转

```text
任务执行
   -> durable insight 进入 memory
   -> 重复成功变成 signal
   -> workflow 提升成 skill
   -> skill 热加载回 runtime
   -> 后续协作更快、更稳
```

这正是 EnterOS 最强的长期价值之一：系统会越来越像一个组织，而不是越来越像一段越来越大的隐藏 prompt。

## EnterOS 内建的自我进化式 Agent Team 架构

很多 agent 系统停留在“runtime 很聪明”。EnterOS 往前走了一步: 它让团队可以**把有效经验写入 durable memory，把稳定 workflow 提升成 skill，把这些改进热加载回 live workspace，并且让整条闭环在平台层可见、可治理、可复用**。

| 对比维度 | 常见自我进化 agent 模式 | EnterOS |
|---|---|---|
| **进化单元** | 单个 agent session 或 runtime | 一个 workspace、一个团队，最终到整张组织图谱 |
| **运维可见面** | 主要隐藏在 agent 内部循环里 | 可被平台、Canvas、activity stream、memory surface、runtime controls 共同观察和治理 |
| **战略结果** | 一个更聪明的 agent | 一个会持续复利、沉淀 durable knowledge 和 governed skills 的 AI 组织 |

### 在 EnterOS 里，这条闭环落在哪些模块

| 核心机制 | EnterOS 对应模块 | 为什么重要 |
|---|---|---|
| **跨 session 的 durable memory** | `workspace/builtin_tools/memory.py`、`workspace-server/internal/handlers/memories.go`、`workspace-server/internal/memory/`（v2 plugin client + namespace resolver）| 不只是持久化，而且是**按 workspace 隔离**的 —— 每次写入都落在 workspace 自己的 `workspace:<id>` namespace 里；当 agent 显式升级到跨 workspace 共享时，可以通过平台 namespace ACL 写到 `team:<root>` 和 `org:<root>` |
| **Cross-session recall** | `workspace-server/internal/handlers/activity.go` 中的 `/workspaces/:id/session-search` | Recall 同时覆盖 activity history 和 memory rows，不需要再造一个隐蔽的新存储层 |
| **从经验里长出技能** | `workspace/builtin_tools/memory.py` 里的 `_maybe_log_skill_promotion` | 从 memory 到 skill candidate 的提升会被显式记录成平台 activity，而不是默默发生在黑盒里 |
| **技能在使用中持续改进** | `workspace/skill_loader/watcher.py`、`workspace/skill_loader/loader.py`、`workspace/main.py` | Skill 改动可以热加载进 live runtime，下一次 A2A 任务就能直接使用，不需要重启 workspace |
| **持久化 skill 生命周期** | `workspace-server/cmd/cli/cmd_agent_skill.go`、`workspace/plugins.py` | Skill 不只是“生成一次”，而是可以 audit、install、publish、plugin 挂载、治理和复用的正式资产 |

### 为什么这在 EnterOS 里更适合团队级系统

1. **学习闭环是 org-aware 的，而不只是 session-aware。**
   Memory 可以按 `LOCAL`、`TEAM`、`GLOBAL` scope 运作，v2 plugin 的 namespace ACL 让每个 workspace 都有清晰的持久边界。

2. **学习闭环是对运维可见的。**
   Promotion events、activity logs、current-task updates、traces、WebSocket fanout 让自我进化进入 control plane，而不是藏在黑盒内部。

3. **学习闭环是可以跨团队复利的。**
   某个 workspace 学出来的稳定 workflow 可以变成受治理的 skill，热加载回 runtime，写进 Agent Card，并继续服务更大的组织层级。

所以 EnterOS 的目标不只是“一个会学习的 agent”，而是**一个会随着工作沉淀出 durable memory 和 reusable procedure、并持续变强的 AI 组织**。

## `main` 分支已经具备什么

### Canvas（v4）

- Next.js 15 + React Flow + Zustand
- **warm-paper 主题系统** —— light / dark / 跟随系统；SSR cookie + nonce'd boot 脚本 + ThemeProvider；终端与代码面板始终保持深色
- drag-to-nest 团队构建
- empty state + onboarding wizard
- template palette
- bundle import/export
- 包含 chat、activity、details、skills、terminal、config、files、memory、traces、events 的 10 个侧栏 tab

### Platform

- Go 1.25 / Gin control plane（80+ HTTP 端点 + Gorilla WebSocket fanout）
- workspace CRUD 和 provisioning（可插拔 Provisioner —— 本地 Docker、生产 EC2 + SSM）
- **A2A 响应路径已收敛为类型化的判别联合（RFC #2967）** —— 冻结 dataclass + 全量 parser；100% 单元测试 + 对抗性 fuzz 覆盖
- registry 与 heartbeat
- 浏览器安全的 A2A proxy
- team expansion/collapse
- activity logs 与 approvals
- secrets 和 global secrets
- files API、terminal、bundles、templates、viewport persistence

### Runtime

- 统一 `workspace/` 镜像；生产环境采用 thin AMI（us-east-2）
- adapter 驱动执行，覆盖 **4 个维护中的 runtime**（Claude Code、Codex、Hermes、OpenClaw）
- Agent Card 注册
- **Memory v2 由 pgvector 支撑** —— 每个 tenant 一个 plugin sidecar，承载 HMA namespace、FTS 与语义召回
- plugin 挂载共享 rules/skills
- 本地 skills 热加载
- coordinator-only delegation 路径

### Ops

- Langfuse traces
- current-task reporting
- pause/resume/restart
- activity streaming
- runtime tiers
- 终端与文件层面的 workspace 直接排障

### SaaS（EnterOS Cloud）

- 多租户运行在云 VM + 每租户 Postgres branch + 每租户隧道（对外不开任何端口）
- WorkOS AuthKit + Stripe Checkout + Customer Portal
- KMS 信封加密（DB / Redis 连接串）；托管 secret store 负责租户 bootstrap
- `tenant_resources` 审计表 + 30 分钟 boot-event-aware reconciler —— 每个基础设施 lifecycle 事件都有记录，每 30 分钟比对 claim 与实际状态

### 在 Claude Code 里直接接入

- 把 EnterOS A2A 流量桥接到本地 Claude Code 会话的 MCP 插件
- 订阅一个或多个 workspace；peer 的消息会以 user-turn 出现，回复会经 EnterOS A2A 路由出去
- 无需公网隧道、无需公开端点 —— 插件启动时自动把每个 watched workspace 注册成 `delivery_mode=poll`，长轮询 `/activity?since_id=…`
- 多租户友好：单次安装即可同时 watch 跨多个 EnterOS 租户的 workspace（`ENTEROS_PLATFORM_URLS` 按 workspace 配置）

## 适合什么团队

EnterOS 特别适合下面这些场景：

- 需要 PM / Dev Lead / QA / Research / Ops 等角色协作的 AI 工程团队
- 不同子团队偏好不同 agent runtime 的组织
- 需要长期 memory 边界和技能沉淀的 agent 系统
- 想把 agent team 作为正式基础设施，而不是零散脚本的内部平台团队

## 架构总览

```text
Canvas (Next.js 15, warm-paper :3000)  <--HTTP / WS-->  Platform (Go 1.25 :8080)  <---> Postgres + Redis
         |                                                           |
         |                                                           +--> Provisioner: Docker (本地) / cloud VMs (生产)
         |                                                           +--> bundles · templates · secrets · KMS
         |
         +------------------------- 展示 ------------------------> workspaces, teams, tasks, traces, events

Workspace Runtime (Python ≥3.11，含 adapter 集合的镜像)
  - 4 个 adapter: Claude Code / Codex / Hermes / OpenClaw
  - Agent Card + A2A server（typed-SSOT 响应路径，RFC #2967）
  - heartbeat + activity + Memory v2（pgvector 语义召回，per-tenant plugin sidecar）
  - skills + plugins + hot reload

EnterOS Cloud（托管 SaaS，独立服务）
  - 每租户 cloud VM + Postgres branch + tunnel
  - WorkOS · Stripe · KMS · managed secret store
  - tenant_resources 审计 + 30 分钟 reconciler
```

## 快速开始

```bash
git clone https://github.com/EnterOS-AI/enter-os-core.git
cd enter-os-core

cp .env.example .env
# 默认值即可在本地启动整套服务。.env.example 里有针对生产部署的
# 安全配置说明（ADMIN_TOKEN、SECRETS_ENCRYPTION_KEY 等）。

./infra/scripts/setup.sh
# 启动 Postgres (:5432)、Redis (:6379)、Langfuse (:3001)
# 以及 Temporal (:7233 gRPC, :8233 UI)，全部挂在共享 Docker
# 网络上。Temporal 默认无鉴权，
# 仅用于本地开发；生产环境必须加 mTLS / API Key。
#
# 同时会根据 manifest.json 拉取所有模板/插件仓库。
# 需要安装 jq：`brew install jq`（macOS）或 `apt install jq`（Debian）。
# 脚本幂等：已经存在内容的目录会被跳过，可以安全重跑。

cd workspace-server
go run ./cmd/server   # 首次启动会自动跑 schema_migrations 里未应用的迁移

cd ../canvas
npm install
npm run dev
```

然后打开 `http://localhost:3000`：

1. 在 empty state 中部署模板，或者创建 blank workspace。
2. 跟着 onboarding guide 进入 `Config`。
3. 在 `Secrets & API Keys` 中添加 provider key。
4. 打开 `Chat` 并发送第一条任务。

## 文档导航

- [文档首页](./docs/index.md)
- [快速开始](./docs/quickstart.md)
- [产品概览](./docs/product/overview.md)
- [系统架构](./docs/architecture/architecture.md)
- [记忆架构](./docs/architecture/memory.md)
- [Platform API](./docs/api-protocol/platform-api.md)
- [Workspace Runtime](./docs/agent-runtime/workspace-runtime.md)
- [Canvas UI](./docs/frontend/canvas.md)
- [本地开发](./docs/development/local-development.md)
- [生态观察](./docs/ecosystem-watch.md) — 值得关注的相邻项目（Holaboss、Hermes、gstack 等）

## 当前范围说明

当前 `main` 已经包含核心平台、Canvas v4（warm-paper 主题）、Memory v2（pgvector 语义召回）、typed-SSOT A2A 响应路径（RFC #2967）、**4 个维护中的正式 adapter**（Claude Code、Codex、Hermes、OpenClaw）、skill lifecycle，以及主要运维面。

**EnterOS Cloud** 提供托管 SaaS 层 —— 多租户编排（cloud VMs + 每租户 Postgres + tunnels）、KMS 信封加密、WorkOS 鉴权、Stripe 计费，以及 `tenant_resources` 审计表加 30 分钟 reconciler。

像 **NemoClaw** 这样的相邻 runtime 路线仍然属于分支级工作，只有合并后才会进入正式支持列表，这里会明确区分。

## License

[Business Source License 1.1](LICENSE) — 版权所有 © 2025 EnterOS。

允许个人、内部与非商业用途。不得使用本作品提供与本产品竞争的商业服务。2029 年 1 月 1 日起转为 Apache 2.0。
