# CPA State 本地中转

[English](README.md) · [下载 Windows 版](https://github.com/dqIndieGames/cpa-state/releases/latest)

面向 Windows 的 Codex 本地中转，提供原生状态窗口、按账号切换通道、按会话查看具体错误。默认普通转发，可选实验性 BPS 通道。

**使用自己的 Codex/ChatGPT 登录与上游权限。** 项目不提供账号、凭据、模型权限或托管服务，与 OpenAI 无隶属关系。程序界面目前为中文，使用文档提供中英双语。

## 主打功能：解决图片与长会话中的反复重试

**看得见具体报错，在中转层处理已知的图片与历史兼容问题。**

| 遇到的问题 | CPA State 的修复与处理 |
| --- | --- |
| **BPS 带图请求报 422** | 将用户图片转换为上游兼容的附件传输格式，保留图片原始字节，不执行历史工具。 |
| **旧图堆积，反复 524 超时** | 在客户端下一次重试时，逐级降低旧图细节；必要时将旧图替换为占位图，保留最近图片。524 减负仅对超过 5 张图片且输入至少 4 MiB 的请求触发。 |
| **BPS 异步工具历史报 400** | 兼容 `functions.exec` 同一调用的后续进度通知，避免合法的追加输出被当成重复工具结果；也兼容客户端移除内部元数据后的请求格式。 |
| **400/422 明确提示上下文超长** | 在适用条件下走同一套有界图片减负流程，依据上游具体原因判断，不只看状态码。 |
| **一直重试，却只看到笼统 HTTP 错误** | 普通与 BPS 模式均按会话展示可用的上游原因、请求 ID；每会话最多 3 类错误，同类合并计数、凭据模式脱敏，成功后标记恢复。 |

这些处理都在 CPA State 中完成，**无需修改 Codex 源码，也不改原始会话 JSONL**。占位图会丢弃本次请求中的旧图视觉细节，并提示模型在需要时重新获取原图。恢复依赖客户端重试，不保证修复所有 400/422/524；具体机制见下方「主要行为」。

## 最快使用：下载 ZIP

1. 安装 Codex，执行 `codex login` 登录自己的账号，保留日常使用的权限和沙箱设置。
2. 从 **Releases** 下载 `cpa-state-windows-amd64.zip`，完整解压到可写目录，不要在压缩包内运行。
3. 双击 **`start.cmd`**。Release 包无需安装 Go。看到状态小窗和托盘图标即已启动；关闭小窗只是隐藏。
4. 将 [examples/codex-config.toml](examples/codex-config.toml) 的 provider 合并到 **用户级** Codex 配置，通常为 `%USERPROFILE%\.codex\config.toml`，设置了 `CODEX_HOME` 时则在该目录。已有 `[features]` 时只合并关闭请求压缩的字段，不要重复创建同名表；保留其他设置和原来的模型选择。
5. 新开终端运行：

```powershell
codex -c 'model_provider="cpa_state_local"' -c features.enable_request_compression=false
```

这只为本次启动选择 CPA。若想默认使用，在用户配置的**顶层、所有 `[表名]` 之前**设置 `model_provider = "cpa_state_local"`。编辑前自行备份；启动器不会自动改写 Codex 配置。

默认地址为 `http://127.0.0.1:17992/backend-api/codex`，使用 HTTP Responses，关闭请求压缩。BPS 不支持 WebSocket。示例文件无需填写 API Key 或 Token。

## 窗口怎么用

- **BPS OFF / ON**：按账号切换普通/BPS 通道，只影响新请求。
- **简约 / 完整**：切换面板布局；点账号名称或箭头进入会话详情。
- **展开原因 / 收起原因**：查看具体诊断；**已恢复**表示后来有响应成功完成。
- 每会话最多 **3 种错误**，同类合并计数，显示上游原因、可用的请求 ID 和恢复状态；CPA 本地校验错误也会显示。
- 凭据模式会脱敏，但错误文案和标题仍可能带个人内容，分享前需要检查。
- 托盘右键选**退出**结束程序；完整重启功能需要 **PowerShell 7（`pwsh.exe`）**，否则退出后再次双击启动即可。
- 重复启动时复用同一路径程序；端口被其他程序占用时说明原因，不自动杀进程。

## 主要行为

- 普通模式转发至 `chatgpt.com`；BPS 模式将 Responses 请求转换后发送至 `bps.openai.com`。认证来自客户端本次请求，项目不内置凭据。
- BPS 兼容客户端工具、用户图片附件、重启后的完整历史和异步 `functions.exec` 进度通知；不会重新执行历史工具。
- BPS 流完整结束并通过校验才标记完成，仅 HTTP 200 不代表生成成功。
- 明确的上下文错误，或图片较多的 524（超过 5 张、输入至少 4 MiB），会在**客户端下一次重试**触发有界减负阶梯：旧图 `original` 降为 `high`，保留最近 5 张，再保留 1 张；之后把旧图替换为有效占位图，保留最近 5 张，再保留 1 张。无效果的级别跳过。
- **占位意味着本次请求丢弃了旧图的视觉细节。** 模型会收到提示，需要旧图细节时应重新获取原图。原始会话文件不改；只有哈希确认历史只追加时才延续减负，修改历史、模型、通道或压缩后重新判定；`/compact` 输入不做该减负改写。
- 重试仍由客户端负责。并非所有 400/422/524 都能修复，普通参数、权限错误不会被当成上下文超长。

## 运行要求与边界

- Windows 10/11 x64；启动可用 Windows PowerShell 5.1 或 PowerShell 7；可选的完整重启助手需要 PowerShell 7。
- 支持 Responses 的 Codex 客户端，以及你自己的有效账号权限。开发验证使用过定制 Codex 0.155.1 local3；其他客户端的多账号、原生标题和工具格式可能不同。
- BPS 为实验性兼容，可能对你的账号不可用，也可能随上游变化。建议先使用 **BPS OFF**；中转不能授予权限或保证模型可用，使用自己账号支持的模型。
- BPS 保留 `low/medium/high/xhigh`；`max/ultra` 映射为 `xhigh` 并显示实际档位。priority/flex 服务等级明确拒绝，default/auto 省略不支持的字段。
- 普通网络走操作系统路由，包括已配置的 VPN/TUN；传输层不使用 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY`。不附带个人代理/VPN 配置。
- 旧版票据签发已废弃并隐藏，不是使用前提。遗留独立代理代码需自行配置 Clash Verge 和 `CPA_PROXY_BASE`、`CPA_PROXY_EXIT` 节点名，可选 `CPA_MIHOMO_EXE`、`CPA_PROXY_INTERFACE`；正常使用保持关闭。

## 账号与本地数据

程序只读发现 `%CODEX_HOME%\auth.json` 和 `accounts\*\auth.json`，默认 home 为 `%USERPROFILE%\.codex`。仅使用系统密钥库登录时，账号可能在客户端发来认证请求后才出现；账号卡片不等于权限验证，认证由上游执行。

运行数据位于 `%LOCALAPPDATA%\cpa-state-relay`：设置、按账号设置、限量错误快照、标题缓存、网络诊断和重启备份。界面及状态接口可能显示姓名、邮箱、会话标题，不要公开该目录或未检查的截图。每账号错误快照最多 1 MiB、500 会话。CPA 不持久保存完整 HTTP 请求和图片；Codex 自己管理会话记录。

BPS 自动标题可能将用户消息最多 4096 字符发往同账号 BPS 标题服务，并在本地缓存标题；正常模型请求也会发送至选定上游。

程序仅绑定本机回环地址，是**本地桌面工具**，不是带用户认证的公网代理。不要通过隧道或反向代理暴露端口；`/api/status` 用于本机诊断。

## 源码启动

安装 [Go 1.26 或更高版本](https://go.dev/dl/)，双击根目录 `start.cmd`，缺少 EXE 时自动构建。也可执行：

```powershell
cd cpa_state_relay
go test ./... -count=1 -timeout 150s
go vet ./...
go build -trimpath -ldflags '-H windowsgui' -o cpa-state-relay.exe .
```

修改源码后需要主动重编译，启动器不会覆盖运行中的 EXE。测试使用本地样例与模拟服务，不使用你的真实登录；其中响应头等待测试故意耗时约 76 秒。

自定义启动示例：

```powershell
pwsh -File .\cpa_state_relay\start.ps1 -Port 17994 -Settings 'C:\CPA Data\settings.json' -AccountHome 'C:\My Codex Home'
```

改端口后同步修改 Codex 的 `base_url`。`-Headless` 不显示界面；`-NoBuild` 在缺 EXE 时直接报错，不自动编译。启动器不结束任何已有进程。

## 常见问题

| 现象 | 处理 |
| --- | --- |
| 缺少 EXE | 下载 Release ZIP，或安装 Go 后从源码启动。 |
| 端口占用 | 自行退出占用程序，或同时修改 CPA 和 Codex 端口。 |
| 没有账号卡片 | 登录 Codex 后发送请求，核对账号目录；不要分享 auth.json。 |
| 401/403、模型权限错误 | 检查自己的登录及模型权限，先试 BPS OFF。 |
| 400/422 | 展开具体原因，区分参数、历史格式与图片问题。 |
| 524 | 检查网络及图片/上下文负载；后续重试可能触发减负，上游故障无法由本地保证修复。 |
| 小窗消失 | 从托盘恢复；关闭小窗不等于退出。 |
| 改源码后没生效 | 退出 CPA 后重新构建，启动器不会自动覆盖已有 EXE。 |

停止使用时退出 CPA，并取消 `model_provider` 覆盖或恢复原 provider，登录文件不受影响。卸载可删除解压目录；检查备份后按需删除 CPA 本地数据目录，保留 Codex 登录及配置文件。

## 许可证与参考

MIT，见 [LICENSE](LICENSE)。依赖许可见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)，Release ZIP 附带依赖许可证原文。

配置依据：[Codex 官方配置参考](https://learn.chatgpt.com/docs/config-file/config-reference)。BPS 行为描述来自本项目实现与验证，不代表上游稳定性承诺。
