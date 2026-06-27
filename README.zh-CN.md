<div align="center">

<p>
  <img src="./docs/assets/branding/enteros-logo.svg#gh-light-mode-only" alt="EnterOS" width="420" />
  <img src="./docs/assets/branding/enteros-logo-white.svg#gh-dark-mode-only" alt="EnterOS" width="420" />
</p>

<h3>首个真正意义上的 AI Agent 操作系统</h3>

<p>
  EnterOS 正在创造 Agent 原生劳动力层：通过统一运行时、组织拓扑和生产级控制面，让 Agent 拥有团队角色、组织架构（职级）、分层记忆与治理能力，进化为具备极大潜力的生产级自动化劳动力。
</p>

<p>
  <a href="https://www.enteros.ai"><b>官网</b></a> ·
  <a href="./docs/quickstart.md"><b>快速开始</b></a> ·
  <a href="./docs/architecture/architecture.md"><b>系统架构</b></a> ·
  <a href="./docs/api-protocol/platform-api.md"><b>API</b></a> ·
  <a href="./README.md"><b>English</b></a>
</p>

<p>
  <a href="LICENSE"><img alt="License: BSL 1.1" src="https://img.shields.io/badge/License-BSL%201.1-orange.svg"></a>
  <a href="https://golang.org/"><img alt="Go 1.25+" src="https://img.shields.io/badge/go-1.25+-00ADD8?logo=go"></a>
  <a href="https://www.python.org/"><img alt="Python 3.11+" src="https://img.shields.io/badge/python-3.11+-3776AB?logo=python"></a>
  <a href="https://nextjs.org/"><img alt="Next.js 15" src="https://img.shields.io/badge/Next.js-15-black?logo=next.js"></a>
  <a href="https://github.com/EnterOS-AI/enter-os-core/stargazers"><img alt="GitHub stars" src="https://img.shields.io/github/stars/EnterOS-AI/enter-os-core?style=social"></a>
</p>

</div>

<!-- TODO：在此放一张 Canvas 产品截图或 GIF —— 这是本 README 性价比最高的一处补充。
<p align="center">
  <img src="./docs/assets/screenshots/canvas.png" alt="EnterOS Canvas" width="900" />
</p>
-->

---

## 为什么是 EnterOS

Agent 正在成为劳动力。但今天的工具仍把它们当成 prompt、chat、script 或 workflow node —— 一个个无法被组织、被治理、被信任投入生产的孤立 demo。

**EnterOS 是首个真正意义上的 AI Agent 操作系统** —— 这个时代缺失的那一层操作层。它给 AI Agent 提供真正的劳动力所需要的一切:岗位角色、汇报关系、记忆边界、运行时契约、审批与生产级控制。做一条 workflow、一个强单体 Agent、一张自定义多 Agent 图,谁都能做;只有 EnterOS 能把这一切作为一个*可治理的组织*来运行 —— 拥有清晰的结构、持久的记忆边界和真正的运维能力。

这正是 EnterOS 要填补的空白 —— 而我们是第一个填上它的人。

|  | 工作流编排 | Agent 框架 | 编码 Agent | **EnterOS** |
|---|:---:|:---:|:---:|:---:|
| 可视化自动化 | ✅ | — | — | ✅ |
| 强运行时语义 | — | ✅ | ✅ | ✅(任意运行时) |
| 组织原生的角色与层级 | — | — | — | ✅ |
| 统一控制面与运维 | — | — | — | ✅ |
| 分层、可复利的记忆 | — | — | — | ✅ |
| 多运行时,统一操作模型 | — | — | — | ✅ |

## 核心能力

- **🧩 角色,而非任务** —— 一个 workspace 就是一个组织角色,拥有稳定的身份、生命周期和记忆边界。它可以从单个 Agent 起步,长成一个受管理的子团队,而不破坏上游集成。
- **🏢 组织架构即拓扑** —— 委派、可见性、审批和记忆都沿着层级自动展开,无需手工连线协作图。
- **🔌 接入任意运行时** —— Claude Code、Codex、Hermes、OpenClaw 在同一套 workspace 契约下并行运行。统一治理,而不强迫每个团队迁到同一运行时。
- **🧠 把记忆当基础设施** —— `LOCAL` / `TEAM` / `GLOBAL` 三级作用域,由 Memory v2 + pgvector 语义召回支撑。共享沿层级进行,而非在全系统泄漏。
- **♻️ 自我进化的团队** —— 沉淀下来的成功经验从记忆晋升为可复用、可热加载的技能(skill),并在平台层可见,而非藏在 Agent 循环内部。
- **🎛️ 真正的控制面** —— 注册中心、心跳、审批、终端、文件、链路追踪、暂停/恢复/重启,以及 WebSocket 实时更新都是一等公民,而非事后补丁。

## 快速开始

```bash
git clone https://github.com/EnterOS-AI/enter-os-core.git
cd enter-os-core

cp .env.example .env
# 默认配置即可在本地启动整套栈。
# 生产加固项(ADMIN_TOKEN、SECRETS_ENCRYPTION_KEY 等)见 .env.example。

# 在共享 Docker 网络上启动 Postgres、Redis、Langfuse、Temporal,
# 并填充模板/插件注册表。需要 jq(`brew install jq`)。
./infra/scripts/setup.sh

# 启动控制面(首次启动自动执行迁移)
cd workspace-server && go run ./cmd/server

# 另开一个终端,启动 Canvas 前端
cd canvas && npm install && npm run dev
```

然后打开 **[http://localhost:3000](http://localhost:3000)**:

1. 从空状态部署一个模板,或创建一个空白 workspace。
2. 跟随引导向导进入 **Config**。
3. 在 **Secrets & API Keys** 中添加 provider key。
4. 打开 **Chat**,发出第一个任务。

> 前置条件、手动部署与排错详见完整的[快速开始指南](./docs/quickstart.md)。

## 系统架构

```text
Canvas (Next.js 15, :3000)  <--HTTP / WS-->  Platform (Go 1.25, :8080)  <--->  Postgres + Redis
        │                                              │
        │                                              ├─ Provisioner:Docker(本地)/ 云 VM(生产)
        │                                              └─ bundles · templates · secrets · KMS
        │
        └──────────── 展示 ────────────> workspaces、teams、tasks、traces、events

Workspace Runtime (Python ≥3.11,内置适配器镜像)
  • 4 个适配器:Claude Code / Codex / Hermes / OpenClaw
  • Agent Card + A2A server(类型化响应路径,经 fuzz 测试)
  • 心跳 · 活动 · Memory v2(pgvector 语义召回)
  • skills · plugins · 热加载

EnterOS Cloud(托管 SaaS,独立产品)
  • 每租户独立云 VM + Postgres 分支 + 私有隧道
  • WorkOS · Stripe · KMS · 托管密钥库
  • tenant_resources 审计表 + 30 分钟对账器
```

更多设计细节见[系统架构](./docs/architecture/architecture.md)与[记忆架构](./docs/architecture/memory.md)。

## 运行时兼容性

EnterOS 不替代下列运行时 —— 它是让它们能*协同运行*的那套系统。

| 运行时 | 状态 | 原生强项 | EnterOS 补充的能力 |
|---|---|---|---|
| **Claude Code** | ✅ 已上 `main` | 真实编码工作流,CLI 原生连续性 | 安全 workspace 抽象、A2A 委派、组织边界、共享控制面 |
| **Codex** | ✅ 已上 `main` | OpenAI Codex CLI 工作流 | 同一套 workspace 契约、委派与运维面 |
| **Hermes 4** | ✅ 已上 `main` | 混合推理、原生工具、结构化输出 | 对接 OpenAI 兼容 API 的 A2A 桥接、多 provider 推导 |
| **OpenClaw** | ✅ 已上 `main` | CLI 原生运行时,自有会话模型 | workspace 生命周期、模板、活动日志、拓扑感知协作 |
| **NemoClaw** | 🚧 分支(`feat/nemoclaw-t4-docker`) | NVIDIA 方向的运行时路径 | 合并后接入同一抽象 —— 尚未进入 `main` |

**多种 Agent 运行时,一套组织级操作系统。**

## 记忆架构为何能复利

大多数项目止步于"我们加了记忆"。EnterOS 把记忆当作分层、受治理的基础设施,并将其变成一个飞轮:

```text
任务执行
  → 把持久洞察写入分层记忆
  → 反复成功成为信号
  → 工作流晋升为可复用技能
  → 技能热加载进运行时
  → 后续工作更快、更可靠
```

| 传统记忆方案 | EnterOS |
|---|---|
| 扁平存储或弱命名空间 | 与层级对齐的 `LOCAL` / `TEAM` / `GLOBAL` 作用域 |
| 共享容易过度暴露 | 共享是显式且结构感知的 |
| 记忆与流程混在一起 | 记忆存持久事实,技能存可复用流程 |
| 每个 Agent 都可能过度授权 | 按 workspace 划分命名空间,缩小爆炸半径 |

最终得到的不是"一个会学习的 Agent",而是*一个随 workspace 不断积累持久记忆与可复用流程而越来越强的组织*。

## `main` 已交付的内容

<table>
<tr>
<td valign="top" width="50%">

**Canvas(v4)**
- Next.js 15 · React Flow · Zustand
- Warm-paper 主题系统(浅色 / 深色 / 跟随系统)
- 拖拽嵌套组队 & 引导向导
- 模板面板 + bundle 导入/导出
- 10 个标签的侧栏:chat、activity、details、skills、terminal、config、files、memory、traces、events

**Platform**
- Go 1.25 / Gin 控制面(80+ 接口 + WebSocket 扇出)
- workspace CRUD 与置备(本地 Docker / 生产云 VM)
- 类型化、经 fuzz 测试的 A2A 响应路径
- 注册中心、心跳、浏览器安全的 A2A 代理
- 活动日志、审批、secrets、files、terminal、templates

</td>
<td valign="top" width="50%">

**Runtime**
- 跨 4 个维护中运行时的适配器驱动执行
- Agent Card 注册
- 基于 pgvector 的 Memory v2(全文检索 + 语义召回)
- 插件挂载的共享规则/技能,可热加载技能

**Ops**
- Langfuse 链路追踪与当前任务上报
- 暂停 / 恢复 / 重启 流程
- 活动流、运行时分层
- 通过终端与文件直接巡检 workspace

**EnterOS Cloud**
- 多租户云 VM + 每租户 Postgres + 私有隧道
- WorkOS AuthKit · Stripe · KMS 信封加密
- `tenant_resources` 审计表 + 30 分钟对账器

</td>
</tr>
</table>

## 为需要"不止 demo"的团队而生

当你需要运行以下场景时,EnterOS 尤其强大:

- 带 PM / Dev Lead / QA / Research / Ops 角色的 AI 工程团队
- 混合运行时组织 —— 一个团队用 Hermes,另一个用 Claude Code
- 需要记忆边界与可复用流程的长期 Agent 组织
- 希望把 Agent 团队作为结构化基础设施(而非临时脚本)对外暴露的内部平台

## 文档

| | |
|---|---|
| [文档首页](./docs/index.md) | [快速开始](./docs/quickstart.md) |
| [产品概览](./docs/product/overview.md) | [系统架构](./docs/architecture/architecture.md) |
| [记忆架构](./docs/architecture/memory.md) | [Platform API](./docs/api-protocol/platform-api.md) |
| [Workspace Runtime](./docs/agent-runtime/workspace-runtime.md) | [Canvas UI](./docs/frontend/canvas.md) |
| [本地开发](./docs/development/local-development.md) | [测试策略](./docs/engineering/testing-strategy.md) |
| [后端能力矩阵](./docs/architecture/backends.md) | [术语表](./docs/glossary.md) |

## 参与贡献

EnterOS 开放共建,欢迎贡献者。请从[本地开发](./docs/development/local-development.md)开始,浏览[待办 issue](https://github.com/EnterOS-AI/enter-os-core/issues),然后提交 PR。较大的改动请先开 issue 讨论方向。

## 社区

- 🌐 官网 —— [enteros.ai](https://www.enteros.ai)
- 🐛 问题与需求 —— [GitHub Issues](https://github.com/EnterOS-AI/enter-os-core/issues)
- 💬 讨论 —— [GitHub Discussions](https://github.com/EnterOS-AI/enter-os-core/discussions)

如果 EnterOS 让你有共鸣,**点一颗 ⭐ 的意义超乎你的想象** —— 这是这个品类被看见的方式。

## 许可证

[Business Source License 1.1](LICENSE) —— 版权所有 © 2025 EnterOS。

个人、内部及非商业用途不受限制。你不得使用本作品提供与之竞争的产品或服务。**2029 年 1 月 1 日**起,许可证转为 **Apache 2.0**。

---

## 创始团队

EnterOS 由以下成员共同打造:

- **Maverick** —— 联合创始人 · 微信 `-MrCui-` · [saitannrinn@gmail.com](mailto:saitannrinn@gmail.com)
- **Hongming Wang** —— 联合创始人 · [hongmingwang@moleculesai.app](mailto:hongmingwang@moleculesai.app)

<div align="center">
<sub>为 Agent 劳动力打造操作系统。</sub>
</div>
