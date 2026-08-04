# Public Release Checklist

## 1. 干净快照

- [ ] 从明确 allowlist 创建新的 staging 目录，不复制当前仓库根目录。
- [ ] 排除 `.release/`、`data/`、`tmp/`、二进制、数据库、`.env`、私钥、日志和浏览器资料。
- [ ] 排除私有 bootstrap、生产切换/回退脚本、内部冷保存和生产收据。
- [ ] 将所有测试凭据替换为明显的非秘密 fixture。
- [ ] 所有文档链接只指向公开快照内文件。

## 2. 法律与治理

- [ ] 确认 Apache-2.0 选择并添加 LICENSE。
- [ ] 审计 Go、npm、图标、字体、截图和扩展依赖许可证。
- [ ] 添加 SECURITY、CONTRIBUTING、Code of Conduct 和 DCO/CLA 决策。
- [ ] 标注第三方商标和非隶属关系。

## 3. 安全

- [ ] 对文件内容和完整 Git 历史执行 secret scan。
- [ ] 搜索生产域名、IP、用户名、容器名、token、cookie、API key 和密钥文件名。
- [ ] 验证默认只监听 loopback、必须提供管理员 token、默认无供应商/目标。
- [ ] 验证配置导出不含明文凭据。
- [ ] 扫描依赖漏洞、容器镜像和 SBOM。

## 4. 构建与测试

- [ ] 从全新 checkout 执行 `go test ./... -count=1`。
- [ ] 执行 `go vet ./...`。
- [ ] 执行前端 locked install 和 production build。
- [ ] 执行浏览器助手合同测试和打包测试。
- [ ] 构建 Linux/Windows 产物并记录 SHA-256。
- [ ] 构建 Docker 镜像并用空数据启动健康检查。
- [ ] 使用内置 NewAPI fixture 做一次远程写关闭的端到端验收。

## 5. 文档验收

- [ ] README 五分钟启动可从空机器复现。
- [ ] API 路由清单与 `internal/httpapi/server.go` 一致。
- [ ] Manifest 和 JSON Connector 示例通过真实 parser 测试。
- [ ] 兼容表明确：Sub2 上游已支持，Sub2 目标写入尚未内置。
- [ ] 不宣称所有魔改站、价格字段或计费单位自动兼容。

## 6. GitHub 发布

- [ ] 使用 `ginsonko/yinziapi-relayops-control-plane`，默认分支启用保护。
- [ ] 配置 CI、Dependabot、CodeQL、secret scanning 和私密漏洞报告。
- [ ] 推送单一已审查的初始提交 SHA。
- [ ] 创建 `v0.1.0` beta tag 和 GitHub Release。
- [ ] Release 包含变更摘要、限制、校验和、SBOM 和升级/回退说明。
- [ ] 发布后从 GitHub 再次全新 clone 并重复 smoke test。
