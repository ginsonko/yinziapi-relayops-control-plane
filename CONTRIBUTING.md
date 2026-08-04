# Contributing to RelayOps

## 开发原则

- 先写合同测试和脱敏 fixture，再改连接器；
- 不在生产上游或生产 NewAPI 上做开发测试；
- 价格和倍率使用 decimal 字符串，不使用浮点近似；
- 网络失败不能覆盖最后可信快照；
- 写操作必须有预览、并发比较、读回和可审计收据；
- 不在日志、错误或测试快照中保存凭据和响应正文。
- Source Connector 与 Target Adapter 必须分离；只读适配成功不代表写入能力可用。
- 未知计费单位必须停止写入，不能用默认值掩盖。

## 环境

- Go 1.23+
- Node.js 20+
- pnpm（通过 Corepack）
- Docker Engine 24+ / Compose v2（集成测试可选）

## 提交前

```bash
go test ./... -count=1
go vet ./...
cd web
pnpm install --frozen-lockfile
pnpm build
```

连接器或目标适配器变更还应运行对应 fixture 集成测试和 `node --test tests/*.test.mjs`。PR 描述需说明协议证据、兼容边界、错误分类、资源上限和回滚方式。

## Commit 与 PR

建议使用清晰的命令式标题。一个 PR 只解决一个协议或行为问题；不要混入格式化全仓库、生成产物、数据库、截图凭据或生产部署文件。

## AI 辅助贡献

可以使用 Codex 等工具，但提交者必须理解并审核生成代码。AI 输入不得包含秘密；输出必须通过相同测试、许可证和安全门槛。PR 应说明是否使用 AI 生成了关键协议代码。推荐只提供脱敏 fixture、相对路径和失败合同测试，不要连接生产目标或授予远程写权限。
