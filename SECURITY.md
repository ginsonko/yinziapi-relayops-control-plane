# Security Policy

## Supported versions

公开 beta 只为 `main` 和最新 beta 标签提供安全修复。早期快照不接收回补。

## Reporting

不要在公开 Issue 中提交 Token、Cookie、密码、数据库、HAR、真实上游响应或服务器信息。请通过仓库的 [Private vulnerability reporting](https://github.com/ginsonko/yinziapi-relayops-control-plane/security/advisories/new) 私密报告漏洞。

## Security model

- 默认监听 `127.0.0.1:18180`；远程访问应通过经过认证的 HTTPS、VPN 或 SSH 隧道；
- 生产模式没有默认密钥，必须设置不少于 32 个字符的随机 `RELAYOPS_ADMIN_TOKEN`；
- 凭据使用独立主密钥加密保存；
- 日志和配置导出不得包含凭据正文；
- 上游 URL 经过同源和 SSRF 检查，默认拒绝回环、私网和云元数据目标；
- `RELAYOPS_ALLOW_LOOPBACK_TARGETS` 仅允许在本地开发模式和回环监听下开启；
- 远程写、后台 worker 和无人值守分别控制，公开构建默认全部关闭；
- 真正可能计费的渠道测试是独立显式操作；
- 容器使用只读根文件系统、最小 capability、PID/CPU/内存限制和有界日志。

## Deployment rules

不要公开裸端口，不要复用 NewAPI 数据库或管理 Token，不要将主密钥放入镜像。备份 SQLite 时必须同时保护主密钥；缺少任一项都不能构成可恢复备份。

## Public contribution rules

任何 fixture 必须脱敏。测试中的 token 使用 `fixture-*` 或 `not-a-secret-*`，不得复用真实格式和值。CI 应执行 secret scan、依赖漏洞扫描、许可证扫描和容器镜像扫描。
