# 项目 Agent 工作流

本目录是根 `AGENTS.md` 的执行细则，适用于计划、实施、审查、修复和交付。普通问答或只读查询无需完整加载。

## 读取路由

- 编写计划或实施 Task：读取 [task-lifecycle.md](task-lifecycle.md) 和 [templates/acceptance-matrix.md](templates/acceptance-matrix.md)。
- 代码审查或修复审查发现：读取 [review-closure.md](review-closure.md)。
- Task 交接或正式交付：读取 [verification-gates.md](verification-gates.md)。
- 新会话交接：使用 [templates/handoff.md](templates/handoff.md)。

## 优先级

从高到低：用户当前明确指令、已批准规格和计划全局约束、根 `AGENTS.md` 与本目录、本机全局工作流、Agent 默认习惯。发生冲突或无法同时满足时必须停止并报告。

## 基本原则

- 一个 Task 是能够独立验收、独立审查、独立提交的最小交付单元。
- 当前 Task 未通过规格审查、质量审查、专项门禁和文档封账，不得进入下一 Task。
- 计划状态、验证证据和用户文档属于完成定义，不是事后补充。
- 测试绿色只证明测试覆盖的行为，不自动证明规格符合性。
