# apipod

[English](README.md)

[APIPod](https://www.apipod.ai) 的命令行客户端：在终端、脚本或 agent 里生成图片和视频、预估一次生成的花费、上传参考素材。

如果你是在 Claude、ChatGPT 这类 MCP 宿主里对话式地使用 APIPod，请继续用官方远程 MCP（`/mcp`）；`apipod` CLI 面向的是终端、脚本、CI，以及只会跑 shell、没有 MCP 能力的 agent。

```bash
apipod auth login
apipod image "a red fox in the snow" -m <model> --aspect 16:9 -o ./out
```

每条命令同时照顾人和程序：默认输出表格、进度走 stderr；加 `--json` 后 stdout 上是恰好一个 JSON 文档，程序按[退出码](#退出码)分支处理。

## 安装

三个渠道安装的是同一个 Go 二进制。

```bash
brew install --cask geniuspath/tap/apipod
```

```bash
npm install -g apipod
```

```bash
pipx install apipod-cli
```

也可以从 [releases 页面](https://github.com/geniuspath/apipod-cli/releases)下载归档并[校验](#校验下载)。

几点说明：

- **npm** —— 二进制来自按平台拆分的包（`@apipod/cli-<os>-<cpu>`），通过 `optionalDependencies` 安装。没有安装脚本，安装过程也不下载任何东西，因此 `npm install --ignore-scripts` 照样可用。
- **pip** —— 包名是 `apipod-cli`。PyPI 上的 `apipod` 属于一个无关项目，而且它同样安装名为 `apipod` 的命令；请用 `pipx` 或 `uv tool install apipod-cli`，给本 CLI 一个独立的环境。
- **Go** —— `go install github.com/geniuspath/apipod-cli/cmd/apipod@latest`（需要 Go 1.24 或更新版本）。

Shell 补全：`apipod completion bash|zsh|fish|powershell`（Homebrew cask 会替你装好）。

## 登录

```bash
apipod auth login
```

默认走浏览器：打开浏览器、请你批准「APIPod CLI」这个应用，然后把拿到的 token 存进系统钥匙串。在没有浏览器的机器上会打印登录 URL，你可以到别处打开；浏览器必须能访问运行 CLI 的那台机器的 `127.0.0.1`。

改用 API key：

```bash
export APIPOD_API_KEY=...        # 给脚本和 CI 用
apipod auth login --with-api-key # 把 key 存入钥匙串；从 stdin 或无回显提示读取
```

刻意不提供 `--api-key` 参数：命令行参数会进 shell history，也会被其他进程看到。

```bash
apipod auth status   # 查看当前用的是哪个凭证（密钥本身打码显示）
apipod auth logout   # 先在服务端吊销 token，再从本机删除
```

`APIPOD_API_KEY` 的优先级高于已存储的凭证。

## 示例

```bash
# 挑一个模型，看看它接受哪些参数
apipod models list --category video
apipod models params <model>

# 这次生成要花多少钱？
apipod cost <model> -d duration=10 -d resolution=1080p

# 生成一张图，等待完成并下载到 ./out
apipod image "a red fox in the snow" -m <model> --aspect 16:9 -o ./out

# 编辑本地图片（会先上传）
apipod image "make it night" -m <model> --image ./photo.png

# 用首帧生成视频，并带模型专属参数
apipod video "she turns and smiles" -m <model> --image ./frame.png \
  --duration 5 --resolution 1080p -x generate_audio=true

# 先拿任务号，稍后再取结果
apipod video "a drone shot over a fjord" -m <model> --no-wait --json
apipod job wait <job-id> -o ./out

# 上传素材，拿到 URL
apipod upload ./photo.png ./clip.mp4 --json

apipod balance
```

### 模型参数

没有专用 flag 的参数都用 `-x key=value` 传入。名字必须是小写加下划线，与 `models params` 打印的完全一致。值只要是合法 JSON 就按 JSON 发送（`-x seed=42`、`-x draft=true`、`-x camera='{"zoom":1.5}'`），否则按字符串发送。`-x key=@./file` 会先上传本地文件再发送其 URL。专用 flag 优先于 `-x`。

`--image`、`--video`、`--audio` 分别填入 `image_urls`、`video_urls`、`audio_urls`。个别模型的输入字段名不同——`models params` 会给出准确名字——这类输入走 `-x`：

```bash
apipod video "extend the shot" -m <model> -x video_url=@./clip.mp4
```

### 给脚本和 agent

```bash
job=$(apipod video "a sunrise" -m <model> --no-wait --max-cost 2 --json | jq -r .job_id)
apipod job wait "$job" -o ./out --json | jq -r '.results[].path'
```

- `--json` 让 stdout 恰好输出一个 JSON 文档，进度和警告走 stderr。失败时 stdout 保持为空，stderr 上是
  `{"error": {"message", "exit_code", "code", "http_status", "job_id"}}`。
- `--max-cost <usd>` 是硬上限；`--yes` 免确认直接批准花费。在没有终端的环境里，估价超过确认阈值时二者必须给其一（见[花费护栏](#花费护栏)）。
- `--no-wait` 立即返回任务号；`--timeout` 限制等待时长（超时退出码 7，任务仍在运行）。
- `--quiet` 静音进度输出；`--no-color` 关闭颜色（`NO_COLOR` 亦可）。

## 命令

| 命令 | 用途 |
| --- | --- |
| `auth login` / `logout` / `status` | 浏览器登录或存入 API key；吊销；查看当前凭证 |
| `models list [--category image\|video\|speech]` | 浏览模型目录 |
| `models params <model>` | 查看模型接受的参数 |
| `cost <model> [-d k=v] [--has-video]` | 报价，附当前余额 |
| `image <prompt> -m <model> [--image] [--aspect] [-n] [-x k=v]` | 生成或编辑图片 |
| `video [prompt] -m <model> [--image] [--video] [--audio] [--duration] [--resolution] [--aspect] [-x k=v]` | 生成视频 |
| `job get <id>` / `job wait <id> [-o dir] [--timeout]` | 查询任务；等待并下载 |
| `upload <file>...` | 上传本地媒体，输出公开 URL |
| `balance [--open]` | 查询余额；`--open` 打开充值页面 |
| `config get` / `set` / `list` | 本地设置 |
| `version`、`completion` | 版本；shell 补全脚本 |

全局参数：`--json`、`--quiet`、`--no-color`、`--debug`、`--no-wait`、`-o/--output <dir>`、`--max-cost <usd>`、`--yes`。

### 退出码

| 退出码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 1 | 通用错误（包括任务失败） |
| 2 | 用法错误 |
| 3 | 认证：未登录，或凭证被拒绝 |
| 4 | 触发限流 |
| 5 | 不存在（模型、任务） |
| 6 | 计费：余额不足、超过 `--max-cost`，或花费未获确认 |
| 7 | 超时 |
| 8 | 校验失败：请求或本地文件被拒绝 |

### 配置

设置保存在用户配置目录的 `config.toml` 里（`apipod config list` 会打印路径）。凭证从不写入这个文件。

| 键 | 含义 |
| --- | --- |
| `defaults.image_model`、`defaults.video_model` | 省略 `-m` 时使用的模型 |
| `defaults.output` | `human`（默认）或 `json` |
| `spend.confirm_above` | 估价超过多少美元需要确认（默认 `1`） |

| 环境变量 | 含义 |
| --- | --- |
| `APIPOD_API_KEY` | API key；优先于已存储的凭证 |
| `APIPOD_BASE_URL` | API 地址（默认 `https://api.apipod.ai`）；必须是 `https`，回环地址除外 |
| `APIPOD_CONSOLE_URL` | Web 控制台地址，自部署时使用 |
| `APIPOD_CONFIG_DIR` | `config.toml` 与兜底凭证文件所在目录 |
| `NO_COLOR` | 关闭颜色 |

## 安全

这个 CLI 的目标是：即使交给一个可能被恶意指令注入的 agent 使用，也是安全的。本节说明它做到与不做到什么。

### 凭证

- **存储。** token 和 key 存入系统钥匙串（macOS 钥匙串、Windows 凭据管理器、Linux 上的 Secret Service）。只有拿不到钥匙串时，才退回写入 `credentials.json`（权限 0600）并给出警告。该文件一旦允许组或其他用户访问、或是指向别处的符号链接，就会被拒绝读取。
- **参数里不收密钥。** 任何 flag 都不接受 key 或 token。即便误把密钥敲进了错误的位置——flag 的值、命令名或参数名——用法错误也不会复述它。
- **绑定 host。** 凭证以签发它的 API 地址为键存储，也只发往那个地址。把 `APIPOD_BASE_URL` 指向别的 host，不会把已存储的凭证发过去。API 客户端不跟随重定向。
- **仅限 https。** 自定义 base URL 必须是 `https`；明文 `http` 只接受 `127.0.0.1`、`::1` 和 `localhost`。
- **不打印。** `auth status` 只显示打码后的值。`--debug` 下，`Authorization` 等敏感头会替换成 `[REDACTED]`，已知的密钥会从每一行日志里移除，OAuth 请求体则完全不记录。
- **环境变量里的 key 由你指挥。** `APIPOD_API_KEY` 会发往 `APIPOD_BASE_URL` 指向的地方。改写 base URL，就是在决定这把 key 去哪里。

### 登录流程

公开客户端 `apipod-cli` 走带 PKCE（S256）的授权码流程。回调监听只绑定 `127.0.0.1` 的随机端口，只接受一次回调，`state` 以常量时间比较，五分钟超时，回调一到就关闭。回调页面是静态的，不回显请求中的任何内容。access token 一小时有效，用轮换的 refresh token 自动续期。`auth logout` 先在服务端吊销两个 token 再删除本地副本；吊销失败时本地照样删除，并提示你去控制台移除该授权。

### 花费护栏

每次生成前，CLI 先向 API 请求估价。

- 估价不超过 `spend.confirm_above`（默认 $1）：直接执行。
- 超过阈值、且在终端里：向你确认。
- 超过阈值、且没有终端：**以退出码 6 拒绝执行**，除非给了 `--yes` 或 `--max-cost`。
- `--max-cost <usd>` 在任何情况下都是上限，`--yes` 不能越过它。它必须是有限的非负数。
- 无法估价时，请求按「需要确认」处理；给了 `--max-cost` 则直接拒绝。

这项检查发生在任何参考文件上传之前、任务创建之前。

估价与实际扣费使用同一批参数。`-x` 和 `-d` 的名字必须小写（`n` 而不是 `N`）：API 解码请求字段时不区分大小写，计价却按精确名字，换个大小写就会出现「按 10 份扣费、按 1 份估价」。无论参考视频由哪个参数携带，都会上报给估价。`callback_url` 永不转发，CLI 自己轮询结果。

护栏限制的是单条命令的花费，不限制命令条数：要给 agent 一个硬上限，请在控制台里给它一把设置了额度的 API key。

每个生成请求都带 `Idempotency-Key`，同一次请求的每次重试复用同一个 key，因此重试不会被计为第二个任务。只有连接错误和 HTTP 429、502、503 会重试。

### 本地文件

- 只读取显式给出的文件。目录、符号链接、设备文件和 FIFO 一律拒绝；不做通配符展开、不递归；每条命令最多 20 个文件。
- 先嗅探前 3 KB，只接受 PNG、JPEG、WebP、GIF、MP3、WAV、M4A、MP4、WebM 和 MOV，与扩展名无关。`apipod upload ~/.ssh/id_rsa` 或 `apipod upload .env` 会在本地被拒。大小上限与服务端一致：图片 20 MB、音频 50 MB、视频 100 MB。
- 一条命令里的所有文件先全部检查，再发送任何请求：有一个被拒，就一个请求都不发。
- 只发送文件名（basename），不发送路径。
- **上传后的文件是公开 URL。** 任何拿到 URL 的人都能打开这个文件。不要上传你不愿公开的内容。

### 下载

结果文件由一个独立的 HTTP 客户端获取：永远不带你的凭证，只走 `https`（重定向也一样）。文件名由 CLI 生成（`<job-id>-<n>.<ext>`），扩展名取自实际内容，服务端无法决定落盘位置。没有 `--force` 时不覆盖已有文件。先写临时文件再原子改名，单文件上限 2 GiB。

### 终端输出

来自服务端的文本（模型名、错误信息、任务字段）在打印前会剥离 ANSI/OSC 转义序列、控制字符和双向覆盖字符。`--json` 输出由 JSON 编码器转义。

### CLI 不做的事

不支付（`balance --open` 只是打开控制台）、不收密码、不发遥测、不检查更新。

### 校验下载

发布归档都列在 `checksums.txt` 里，该文件用 [cosign](https://github.com/sigstore/cosign) 以 release workflow 的身份签名。不存在可以被偷走的签名私钥。

```bash
cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/geniuspath/apipod-cli/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

```bash
sha256sum --check --ignore-missing checksums.txt
```

每个归档还带有 GitHub 构建来源证明（provenance attestation）和 SPDX SBOM：

```bash
gh attestation verify apipod_<version>_<os>_<arch>.tar.gz --repo geniuspath/apipod-cli
```

npm 包带 provenance 发布（`npm audit signatures`），PyPI wheel 通过 Trusted Publishing 发布。

报告安全漏洞请看 [SECURITY.md](SECURITY.md)。

## 开发

```bash
go test ./...
go vet ./...
go run ./cmd/apipod --help
```

对接本地后端：

```bash
APIPOD_BASE_URL=http://127.0.0.1:8080 go run ./cmd/apipod auth login
```

发布演练（需要 [GoReleaser](https://goreleaser.com) v2.16 或更新版本）：

```bash
goreleaser check
```

```bash
goreleaser release --snapshot --clean --skip=sign,sbom
```

装好 `cosign` 和 `syft` 后去掉 `--skip=sign,sbom`。之后用 `node scripts/build-npm.mjs` 和 `python3 scripts/build_wheels.py` 从 `dist/` 组装 npm 包和 wheel；见 [npm/README.md](npm/README.md) 与 [python/README.md](python/README.md)。

## 许可证

[MIT](LICENSE)
