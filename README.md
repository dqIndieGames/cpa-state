# CPA State

[中文说明](README.zh-CN.md) · [Download Windows release](https://github.com/dqIndieGames/cpa-state/releases/latest)

A Windows localhost relay for Codex, with a native status window, per-account routing, and per-conversation error details. Ordinary forwarding is the default; an optional experimental BPS bridge is available.

**Bring your own Codex/ChatGPT login and upstream access.** This project provides no accounts, credentials, model entitlement, or hosted service. It is an independent project, not an official OpenAI product. The application UI currently uses Chinese; this guide includes the relevant label translations.

## Highlights: fixes for image and long-session retry loops

**See why a request failed, and recover from known image and history compatibility problems directly in the relay.**

| Problem | What CPA State adds |
| --- | --- |
| **422 after attaching images in BPS** | Carries user images through a compatible attachment envelope, preserving the image bytes. No historical tool is executed. |
| **524 timeouts with a large image history** | On the client's next retry, progressively reduces older image detail and, if needed, replaces older images with placeholders while keeping recent images. The 524 path requires more than five images and at least 4 MiB of input. |
| **400 from asynchronous tool history in BPS** | Adapts recognized `functions.exec` progress notifications that share a call ID, so valid later output is not rejected as a duplicate tool result. Also handles the client wire format without internal metadata. |
| **400/422 explicitly reporting context overflow** | Uses the same bounded image-recovery ladder when applicable, based on the upstream reason rather than the status code alone. |
| **Repeated retries with only a generic HTTP error** | Shows available upstream reasons and request IDs in conversation details for both ordinary and BPS modes. Keeps at most three error groups per conversation, merges repeats, redacts credential patterns, and marks recovery. |

These fixes run in CPA State; **no Codex source changes or edits to original session JSONL files are required**. Image placeholders discard older visual detail from the outgoing request; the model is told to request originals when needed. Recovery runs on client retries and does not guarantee a fix for every 400/422/524. See [What the relay does](#what-the-relay-does) for the exact behavior.

## Quick start: Windows ZIP

1. Install Codex and sign in using your own account (`codex login`). Keep your normal permissions and sandbox settings.
2. Download `cpa-state-windows-amd64.zip` from **Releases** and extract the entire ZIP to a writable folder. Do not run from inside the ZIP.
3. Double-click **`start.cmd`**. No Go installation is needed for the release ZIP. A status panel and tray icon appear; closing the panel only hides it.
4. Add the provider from [examples/codex-config.toml](examples/codex-config.toml) to your **user-level** Codex config, normally `%USERPROFILE%\.codex\config.toml` (or `%CODEX_HOME%\config.toml`). Merge the compression setting into an existing `[features]` table instead of duplicating that table. Preserve all unrelated settings and your existing model selection.
5. Open a new terminal and run:

```powershell
codex -c 'model_provider="cpa_state_local"' -c features.enable_request_compression=false
```

This selects the relay for that invocation. To make it the default, set `model_provider = "cpa_state_local"` at the **top level**, before any `[table]`, in the user config. Back up your config before editing. CPA's launcher does not edit it for you.

Default endpoint: `http://127.0.0.1:17992/backend-api/codex`. HTTP Responses is required; BPS does not support WebSocket requests. Keep request compression disabled. Do not add API keys or access tokens to the example file.

## Using the window

| UI label | Meaning |
| --- | --- |
| BPS OFF / ON | Route new Responses requests for this account through ordinary forwarding / experimental BPS |
| 简约 / 完整 | Compact / full panel |
| 会话详情 | Conversation details; click the account name or arrow |
| 展开原因 / 收起原因 | Expand / collapse the error details |
| 已恢复 | A later response completed successfully |
| 复制 Session | Copy the conversation identifier |

Each conversation keeps at most **three error groups**. Repeated errors are counted instead of appending endless logs. Details show upstream diagnostics, request IDs when supplied, and recovery status. Local BPS validation errors are also shown. Credential patterns are redacted, but diagnostics and titles may still contain personal text: review them before sharing.

Right-click the tray icon to exit (`退出`). The full-restart action requires **PowerShell 7 (`pwsh.exe`)**. Otherwise exit and double-click `start.cmd` again. Repeated launches reuse an instance from the same executable; a port owned by another program is reported and left untouched.

## What the relay does

- Ordinary mode forwards to `chatgpt.com`; BPS mode bridges Responses requests to `bps.openai.com`. Credentials are taken from the client's request and sent to the selected upstream, not embedded in this project.
- BPS handles client tool declarations/results, user-image attachment compatibility, completed tool history after restart, and asynchronous `functions.exec` notifications. The relay does not execute historical tool calls.
- Only a successfully completed and validated BPS stream is marked complete. HTTP 200 by itself does not prove a successful generation.
- Image recovery activates on explicit context errors, or on 524 timeouts for image-heavy input (more than five images and at least 4 MiB). On the **client's next retry**, it follows a bounded ladder: lower older `original` detail while keeping the newest five, then one; replace older images with valid placeholders while keeping five, then one. Ineffective steps are skipped.
- Placeholder recovery **removes old visual detail from that request**. The model is told about the omissions and should request originals when necessary. Original session files are unchanged. Recovery carries across verified append-only history and resets for changed history/model/route. It does not rewrite `/compact` inputs.
- Retries remain the client's responsibility. This relay does not promise that every 400/422/524 is recoverable, and does not treat arbitrary validation or permission errors as context overflow.

## Requirements and compatibility

- Windows 10/11 x64; Windows PowerShell 5.1 or PowerShell 7 for startup. PowerShell 7 is required for the optional full-restart helper.
- A Responses-capable Codex client and your own valid upstream access. Development used a customized Codex 0.155.1 local3 client; account switching, automatic title requests, and some tool formats can differ in other clients.
- BPS is experimental and may be unavailable to your account or change without notice. Start with **BPS OFF**. The relay cannot grant permissions or guarantee model availability. Choose a model your account supports.
- BPS preserves `low/medium/high/xhigh`; `max/ultra` map to `xhigh` and the UI reports the actual level. BPS rejects priority/flex service tiers; default/auto omit the unsupported field.
- Ordinary networking follows OS routing, including a configured VPN/TUN. `HTTP_PROXY`, `HTTPS_PROXY`, and `ALL_PROXY` are not used by its transport. No personal proxy or VPN profile is included.
- Legacy ticket issuance controls are deprecated and hidden. They are not required. The optional legacy independent-proxy code requires your own Clash Verge setup and explicit `CPA_PROXY_BASE` / `CPA_PROXY_EXIT` node names; `CPA_MIHOMO_EXE` and `CPA_PROXY_INTERFACE` are optional overrides. Leave it off for normal use.

## Accounts and local data

CPA reads `%CODEX_HOME%\auth.json` and `accounts\*\auth.json` when present (default home: `%USERPROFILE%\.codex`). These files are **read-only inputs**. Keyring-only logins may not appear until the client sends an authenticated request. Account cards do not validate entitlement; the upstream authenticates each request.

Local data is under `%LOCALAPPDATA%\cpa-state-relay`: settings, per-account settings, bounded error snapshots, title cache, network diagnostics, and restart backups. The window/status API can show account names, emails and conversation titles; do not publish that directory or unreviewed screenshots. Runtime error snapshots are bounded to 1 MiB and 500 conversations per account. The relay does not persist complete HTTP request bodies or images; Codex manages its own transcripts.

BPS automatic title generation may send up to 4096 characters of a user message to the same account's BPS title service. Titles are cached locally. Upstream requests, including ordinary model prompts, necessarily leave your machine.

The HTTP server binds to loopback only. It is a **local desktop tool**, not an authenticated multi-user/public proxy. Do not expose its port through a tunnel or public reverse proxy. `/api/status` is intended for local diagnostics.

## Build from source

Install [Go 1.26 or newer](https://go.dev/dl/). On Windows, double-click `start.cmd`; it builds the executable only if it is absent. Alternatively:

```powershell
cd cpa_state_relay
go test ./... -count=1 -timeout 150s
go vet ./...
go build -trimpath -ldflags '-H windowsgui' -o cpa-state-relay.exe .
```

After changing source, rebuild explicitly; the launcher does not replace a running executable. Tests use local fixtures and mock servers, not your real login. One header-wait test intentionally takes about 76 seconds.

Optional launch parameters:

```powershell
pwsh -File .\cpa_state_relay\start.ps1 -Port 17994 -Settings 'C:\CPA Data\settings.json' -AccountHome 'C:\My Codex Home'
```

If you change the port, update the Codex provider's `base_url` to match. `-Headless` skips the UI; `-NoBuild` fails instead of compiling when the EXE is absent. The launcher never terminates an existing process.

## Troubleshooting

| Symptom | Action |
| --- | --- |
| Missing executable | Use the release ZIP, or install Go and rerun from the source checkout. |
| Port occupied | Exit the other service yourself, or select a different matching relay/client port. |
| No account card | Log in to Codex and send a request; check the account home without sharing `auth.json`. |
| 401/403 or model access error | Verify your own login and model access; try BPS OFF. |
| 400/422 | Expand the exact reason. Unsupported parameters, malformed history, and image issues need different fixes. |
| 524 | Check connectivity and image/context load. The next retry may invoke image recovery; upstream outages remain upstream failures. |
| Window disappeared | Closing hides it; restore it from the tray icon. |
| Updated source but old behavior | Exit CPA and rebuild; existing EXEs are not automatically overwritten. |

To stop using CPA, exit it and remove the `model_provider` override (or use your previous provider). Login files remain unchanged. To uninstall, remove the extracted folder; optionally remove CPA's local-data directory after reviewing backups. Keep your Codex authentication and config files.

## License and references

MIT — see [LICENSE](LICENSE). Third-party dependency licenses are included in release ZIPs and listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

Codex configuration reference: [official documentation](https://learn.chatgpt.com/docs/config-file/config-reference). BPS compatibility behavior above describes this implementation and its tests, not an upstream stability guarantee.
