# Agent Workflow Governance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 建立项目共享与本机全局两层 Agent 工作流，并修正当前 v0.6 文档状态。

**Architecture:** 项目根 `AGENTS.md` 作为强制入口，详细规则放在可版本化的 `docs/agent-workflows/`；本机 `~/.codex/AGENTS.md` 只保存跨项目通用原则。每个 Task 用主提交交付内容，再用紧随其后的状态提交回填实际 commit SHA，避免提交自引用。

**Tech Stack:** Markdown、Git、Codex 全局 `AGENTS.md`

## Global Constraints

- 用户当前指令、已批准规格、项目规则、全局规则依次优先；冲突时停止并报告。
- 每个 Task 必须完成文档状态更新和审查后才能进入下一 Task。
- 用户可见行为变化必须同步 README；内部工作流 Task 必须记录 README 不适用及原因。
- 仓库修改使用独立提交；本机全局文件不进入任何仓库。
- 纯文档任务复用当前有效的代码测试证据，只运行 Markdown/Git/引用一致性检查。

---

### Task 1: 项目 Agent 工作流

**Files:**
- Create: `docs/agent-workflows/README.md`
- Create: `docs/agent-workflows/task-lifecycle.md`
- Create: `docs/agent-workflows/review-closure.md`
- Create: `docs/agent-workflows/verification-gates.md`
- Create: `docs/agent-workflows/templates/acceptance-matrix.md`
- Create: `docs/agent-workflows/templates/handoff.md`
- Modify: `AGENTS.md`
- Modify: `docs/superpowers/plans/2026-08-14-agent-workflow-governance.md`

**Produces:** 项目内所有 Agent 可发现、可复制执行的工作流和模板。

- [ ] **Step 1: 创建项目工作流文档**

文档分别定义入口与优先级、Task 生命周期、审查闭环、三级门禁、验收矩阵和交接模板；不重复项目根已有的具体工具命令。

- [ ] **Step 2: 更新项目入口**

根 `AGENTS.md` 规定在计划、实施、审查和交付任务中按需读取 `docs/agent-workflows/README.md`，并明确项目规则高于本机全局规则。

- [ ] **Step 3: 验证引用和格式**

Run:

```bash
for f in docs/agent-workflows/README.md docs/agent-workflows/task-lifecycle.md docs/agent-workflows/review-closure.md docs/agent-workflows/verification-gates.md docs/agent-workflows/templates/acceptance-matrix.md docs/agent-workflows/templates/handoff.md; do test -s "$f"; done
rg -n "docs/agent-workflows/README.md" AGENTS.md
git diff --check
```

Expected: 所有文件存在且非空，入口引用存在，差异检查通过。

- [ ] **Step 4: 提交主变更**

Commit message: `docs: add project agent workflows`

- [ ] **Step 5: 回填 Task 状态并封账**

在本计划记录主提交 SHA、验证证据和 `README：不适用（仅内部 Agent 工作流，无用户功能变化）`，提交消息为 `docs: record agent workflow task 1`。封账提交完成前不得进入 Task 2。

---

### Task 2: 本机全局工作流入口

**Files:**
- Modify outside repository: `~/.codex/AGENTS.md`
- Create outside repository: `~/.codex/workflows/development/README.md`
- Create outside repository: `~/.codex/workflows/development/task-lifecycle.md`
- Create outside repository: `~/.codex/workflows/development/review-closure.md`
- Create outside repository: `~/.codex/workflows/development/verification-gates.md`
- Create outside repository: `~/.codex/workflows/development/templates/acceptance-matrix.md`
- Create outside repository: `~/.codex/workflows/development/templates/handoff.md`
- Modify: `docs/superpowers/plans/2026-08-14-agent-workflow-governance.md`

**Produces:** 不依赖当前仓库、可供其他项目 Agent 使用的本机通用规则。

- [ ] **Step 1: 备份现有全局入口**

在写入前保存 `~/.codex/AGENTS.md` 的本地备份，备份不进入 Git。

- [ ] **Step 2: 创建本机通用工作流**

只保留跨项目原则，不包含 Go 1.22、当前仓库路径、v0.6 或本项目命令。全局入口指向 `~/.codex/workflows/development/README.md`。

- [ ] **Step 3: 验证全局文件**

检查全部文件非空、入口引用正确、无当前项目专有字符串；展示入口差异供审阅。

- [ ] **Step 4: 回填 Task 状态并封账**

本机文件不提交；在项目计划记录修改路径、备份路径、验证证据和 `README：不适用（本机全局治理，无项目用户功能变化）`，以 `docs: record global workflow task` 提交状态。封账前不得进入 Task 3。

---

### Task 3: 当前项目文档状态审计与修正

**Files:**
- Modify: `docs/superpowers/specs/2026-08-13-v0.6-monitor-auto-recovery-design.md`
- Modify: `docs/superpowers/plans/2026-08-13-v0.6-monitor-auto-recovery.md`
- Modify: `docs/superpowers/specs/2026-08-14-agent-workflow-governance-design.md`
- Modify if audit finds a user-facing gap: `README.md`
- Modify: `docs/superpowers/plans/2026-08-14-agent-workflow-governance.md`

**Produces:** 与已合并实现一致的设计/计划状态和可追溯提交记录。

- [ ] **Step 1: 更新 v0.6 设计状态**

将“待实施”更新为已实施并合并，记录合并提交 `b167df1`；不改写历史设计契约。

- [ ] **Step 2: 更新 v0.6 实施计划记录**

在计划顶部增加执行记录表，逐 Task 记录实际实现提交及后续修复范围。历史步骤不伪造逐步执行证据；以“计划基线已完成、最终门禁已通过、详细修复提交见记录”的方式归档。

- [ ] **Step 3: 更新治理设计状态与自引用规则**

将治理设计更新为已批准/实施，并把“不可能在同一提交中写入自身 SHA”明确为“主提交 + 状态封账提交”。

- [ ] **Step 4: 审计 README**

核对 v0.6 monitor、health remote、安装器测试和发布流程。若已一致，计划记录 `README：无需更新（已覆盖当前用户可见行为）`；若有缺口，同 Task 修改。

- [ ] **Step 5: 验证并提交主变更**

Run:

```bash
rg -n "状态：|b167df1|v0.6|proxyctl monitor" docs/superpowers/specs docs/superpowers/plans README.md
git diff --check
```

Commit message: `docs: align workflow and v0.6 status`

- [ ] **Step 6: 回填最终状态并封账**

记录 Task 3 主提交 SHA、验证证据、README 结论和本计划整体完成状态，提交消息为 `docs: complete agent workflow rollout`。提交后验证工作区为空。

## Final Verification

本轮仅修改 Markdown 和本机 Agent 配置，不改代码、测试、依赖、CI 或运行配置。复用 `70344bf` 后 v0.6 的 Go 1.22 race/vet 与 ShellCheck/安装器测试证据；本轮最终执行：

```bash
git diff --check
git status --porcelain
git log --oneline --decorate -8
```

Expected: 差异检查通过，工作区干净，三个 Task 均有主交付或本机修改证据及状态封账记录。
