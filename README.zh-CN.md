<div align="center">

<p>
  <img src="./docs/assets/branding/enteros-logo.svg" alt="EnterOS" width="420" />
</p>

<p>
  <a href="./README.md">English</a> | <a href="./README.zh-CN.md">中文</a>
</p>

<h3>首个 AI Agent 原生团队操作系统</h3>

<p>
  EnterOS 把 AI Agent 组织成可生产运行的自动化劳动力：团队角色、职级关系、分层记忆、治理、runtime 选择和实时运维，统一在一个控制面里。
</p>

<p>
  <a href="https://www.enteros.ai"><img alt="官网: enteros.ai" src="./docs/assets/branding/cta-website.svg" height="40"></a>
  <a href="#快速开始"><img alt="快速开始: 本地运行" src="./docs/assets/branding/cta-quick-start.svg" height="40"></a>
  <a href="./docs/architecture/architecture.md"><img alt="系统架构: 查看设计" src="./docs/assets/branding/cta-architecture.svg" height="40"></a>
</p>

[![License: BSL 1.1](https://img.shields.io/badge/License-BSL%201.1-orange.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.25+-00ADD8?logo=go)](https://golang.org/)
[![Python Version](https://img.shields.io/badge/python-3.11+-3776AB?logo=python)](https://www.python.org/)
[![Next.js](https://img.shields.io/badge/Next.js-15-black?logo=next.js)](https://nextjs.org/)

<p>
  <strong>Frameworks build agents. EnterOS builds the organization they work in.</strong>
</p>

<p>
  <a href="./docs/index.md"><strong>文档</strong></a> ·
  <a href="./docs/quickstart.md"><strong>快速开始</strong></a> ·
  <a href="./docs/architecture/architecture.md"><strong>系统架构</strong></a> ·
  <a href="./docs/api-protocol/platform-api.md"><strong>Platform API</strong></a> ·
  <a href="#创始人与早期测试用户群"><strong>创始人 / 测试群</strong></a>
</p>

</div>

---

## 为什么是 EnterOS

AI Agent 正在从工具变成劳动力。但今天大多数技术栈仍然把 Agent 当成一次性 chat、script、workflow node，或者某个 framework 里的 demo。

EnterOS 补上的，是 Agent 进入生产系统之后真正缺失的操作层：Agent 有长期角色、汇报关系、记忆边界、凭据、审批、runtime contract、trace，以及可以重启和治理的生产级工作流。

如果你只是在做一个 Agent，framework 可能已经够了。如果你要做一个 AI 组织，你需要的是操作系统。

## EnterOS 提供什么

| 层级 | 实际意味着什么 |
|---|---|
| **Agent 原生组织模型** | Workspace 是长期存在的团队角色，不是一次性的任务节点。一个角色可以先是单 Agent，后续扩成子团队，外部身份仍然稳定。 |
| **组织图就是运行拓扑** | 委派、可见性、审批和 memory sharing 默认沿层级流动，而不是手工维护一张脆弱的 graph。 |
| **异构 runtime 控制面** | Claude Code、Codex、Hermes、OpenClaw 可以共用同一个 workspace contract 和统一运维面。 |
| **可以安全复利的分层记忆** | `LOCAL`、`TEAM`、`GLOBAL` memory scope 让长期知识与组织边界对齐。 |
| **Agent 团队的生产级运维** | Canvas、registry、heartbeat、activity logs、traces、terminal、files、pause/resume、restart、WebSocket updates 都是一等能力。 |
| **Memory 到 Skill 的演化闭环** | 有效经验可以从 durable memory 逐步沉淀成可复用、可热加载的 skills，让后续工作更快、更稳。 |

## 适合谁

EnterOS 面向已经跨过“单个惊艳 Agent”阶段的团队：

- 正在构建 PM、Dev Lead、QA、Research、Ops、Support 等 Agent 角色的 AI 工程团队
- 希望把 Agent 当成内部平台基础设施管理的平台团队
- 想把 Agent workflow 产品化、标准化、可治理的产品团队
- 在大规模使用 Agent 前，需要 memory、凭据、审批和执行边界的企业团队
- 同时比较多种 runtime，不想被单一 agent framework 锁死的 builder

## `main` 分支已经交付什么

| 模块 | 当前能力 |
|---|---|
| **Canvas** | Next.js 15 + React Flow canvas、drag-to-nest team、template deployment、onboarding、bundle import/export，以及包含 chat、activity、details、skills、terminal、config、files、memory、traces、events 的 10-tab workspace panel。 |
| **Control plane** | Go 1.25 / Gin 后端，提供 workspace CRUD、registry、A2A proxy、approvals、secrets、global secrets、files API、terminal access、viewport persistence 和 Gorilla WebSocket fanout。 |
| **Runtime layer** | Python workspace runtime，当前 `main` 维护 Claude Code、Codex、Hermes、OpenClaw 四个 adapter，共用 workspace contract。 |
| **Memory** | Memory v2，基于 pgvector semantic recall、full-text search、per-workspace namespace，以及结构感知的 `LOCAL` / `TEAM` / `GLOBAL` scope。 |
| **Skills** | 本地 `SKILL.md` packages、plugin-mounted shared skills/rules、audit/install/publish helper，并支持热加载到 live workspace。 |
| **Operations** | Langfuse traces、current-task reporting、pause/resume/restart flows、runtime tiers、activity streaming，以及直接检查 workspace 的 terminal 和 files。 |
| **EnterOS Cloud** | 托管 SaaS 形态：cloud VM orchestration、per-tenant Postgres、tunnels、WorkOS AuthKit、Stripe billing、KMS envelope encryption 和 tenant resource reconciliation。 |

## Runtime 兼容性

EnterOS 不是要替代 agent framework，而是给它们一个共同的组织级 operating model。

| Runtime / 架构 | 当前仓库状态 | 原生优势 | EnterOS 补上的能力 |
|---|---|---|---|
| **Claude Code** | `main` 已支持 | CLI-native coding workflow | Workspace 边界、A2A delegation、共享 secrets、traces 和团队运维。 |
| **Codex** | `main` 已支持 | OpenAI Codex CLI workflow | 与其他 workspace 共用 control plane、memory model 和协作契约。 |
| **Hermes** | `main` 已支持 | 混合推理、tools、OpenAI-compatible API path | A2A bridge、provider derivation、workspace lifecycle 和团队级可见性。 |
| **OpenClaw** | `main` 已支持 | CLI-native runtime 和 session model | Templates、activity logs、拓扑感知协作和平台运维。 |
| **NemoClaw** | 分支级工作 | NVIDIA 方向 runtime 路线 | 计划在合并后进入同一抽象层；当前不声明为 `main` 功能。 |

## 架构

```text
Canvas (Next.js 15, React Flow, Zustand)
  |
  | HTTP / WebSocket
  v
Platform Control Plane (Go 1.25, Gin, Postgres, Redis)
  |
  | provisioning, registry, secrets, files, events, A2A proxy
  v
Workspace Runtime (Python 3.11+)
  |
  | adapters: Claude Code / Codex / Hermes / OpenClaw
  v
Agent Team
  |
  | memory, skills, traces, terminal, files, activity
  v
Operational Feedback Loop
```

核心思想很简单：**多种 agent runtime，共用一个 Agent 原生组织系统。**

## 快速开始

```bash
git clone https://github.com/EnterOS-AI/enter-os-core.git
cd enter-os-core
./scripts/dev-start.sh
```

这个脚本会启动本地 stack，包括 Postgres、Redis、Langfuse、ClickHouse、Temporal、Go control plane 和 Next.js canvas。

然后打开 [http://localhost:3000](http://localhost:3000)：

1. 在 **Config -> Secrets & API Keys -> Global** 添加模型 key。
2. 部署一个 template，或创建 blank workspace。
3. 打开 **Chat**，发送第一条任务。

更多依赖、手动启动和 remote-agent 路径，请阅读 [Quickstart Guide](./docs/quickstart.md)。

## 技术栈

| 层级 | Stack |
|---|---|
| **Frontend** | Next.js 15, React 19, React Flow, Zustand, Tailwind CSS, Radix UI, xterm.js |
| **Backend** | Go 1.25, Gin, Gorilla WebSocket, Postgres, Redis |
| **Runtime** | Python 3.11+, workspace adapters, A2A server, plugin-mounted skills |
| **Memory** | pgvector, full-text search, per-workspace namespaces, scoped recall |
| **Ops** | Docker Compose, Langfuse, Temporal, traces, runtime tiers, restart flows |
| **Cloud** | Cloud VM provisioning, per-tenant Postgres, tunnels, WorkOS, Stripe, KMS |

## 创始人与早期测试用户群

EnterOS 由创始人 **Mr. Cui** 推动，目前正在邀请真正构建 Agent 团队、内部自动化平台和生产级 AI 劳动力系统的早期测试用户。

如果你想参与测试、反馈产品，或了解 EnterOS Cloud 托管方案，可以联系：

- 微信：`-MrCui-`
- 邮箱：[saitannrinn@gmail.com](mailto:saitannrinn@gmail.com)

## 文档

- [文档首页](./docs/index.md)
- [快速开始](./docs/quickstart.md)
- [Architecture Overview](./docs/architecture/overview.md)
- [System Architecture](./docs/architecture/architecture.md)
- [Memory Architecture](./docs/architecture/memory.md)
- [Platform API](./docs/api-protocol/platform-api.md)
- [Workspace Runtime](./docs/agent-runtime/workspace-runtime.md)
- [Canvas UI](./docs/frontend/canvas.md)
- [Local Development](./docs/development/local-development.md)
- [Testing Strategy](./docs/engineering/testing-strategy.md)
- [Glossary](./docs/glossary.md)

## 当前边界

当前 `main` 分支已经包含核心平台、Canvas、Memory v2、typed A2A response path、四个维护中的 runtime adapters、skill lifecycle，以及本地运行 Agent 团队所需的主要运维面。

EnterOS Cloud 是面向团队的托管 SaaS 形态，提供 hosted orchestration、tenant isolation、billing、authentication、encrypted secrets 和托管基础设施生命周期。

NemoClaw 等相邻 runtime 工作仍然保持在分支级，合并前不会在 README 中作为 `main` 能力声明。

## License

[Business Source License 1.1](LICENSE) — copyright © 2025 EnterOS.

个人、内部和非商业用途不受限制。你不能使用本 Licensed Work 提供竞争性产品或服务。2029 年 1 月 1 日后，许可证将转换为 Apache 2.0。
