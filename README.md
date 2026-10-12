# PostfixPolicy

用于 moemail 的 SMTP 邮件中转服务。接收完整邮件，按信封发件人、收件人和部署标记查询 PostgreSQL 路由，再经指定 SMTP 中继或收件人域名的 MX 投递。它不实现 Postfix `name=value` 策略检查协议。

## 运行

从源码构建建议使用 Go 1.26.1，与 CI 和容器构建版本一致。Linux 本地运行：

```sh
cp config.example.json config.json
# 填写数据库、主机名、证书路径等配置，并建立 QueuePath 目录
make
./build/policy -conf ./config.json
```

也可下载 Release 中对应架构的二进制，验证校验和后使用。以 amd64 为例：

```sh
chmod +x postfixpolicy-linux-amd64
./postfixpolicy-linux-amd64 -conf ./config.json
```

以 `config.example.json` 为模板填写配置，事先建立队列目录，并配置入站 STARTTLS 证书。`QueuePath` 必须位于支持原子重命名、文件/目录同步和文件锁的本地 Linux 文件系统上；同一目录只允许一个服务实例使用。

默认监听配置中的地址，不提供入站 SMTP AUTH 或客户端访问白名单。只允许受信任的上游连接：示例绑定回环地址，远端 Postfix 接入应通过网络访问控制限制来源。

## GitHub Actions 与发布

[build-release.yml](.github/workflows/build-release.yml) 沿用 Attendance 的发布方式。这里的自动发布指创建 GitHub Release 和推送 GHCR 镜像；更新运行中的服务器由运维执行。

| 触发方式 | 执行内容 |
| --- | --- |
| Pull Request、推送 `main` / `master` / `release/**`、在分支手动运行 | `make check`，构建 Linux amd64/arm64 二进制，上传 Actions 构建产物 |
| 推送 `vX.Y.Z` 标签 | 上述检查与构建，发布双架构 GHCR 镜像，验证镜像清单后创建正式 Release |
| 推送 `vX.Y.Z-rc.1` 等预发布标签 | 同上，Release 标记为预发布，镜像不更新 `latest` |

发布资产包含 `postfixpolicy-linux-amd64`、`postfixpolicy-linux-arm64`、`config.example.json`、`compose.yaml`、`README.md` 和 `SHA256SUMS`。全部下载后运行 `sha256sum -c SHA256SUMS` 验证完整性。

镜像地址为 `ghcr.io/<仓库所有者>/<仓库名>`，名称统一转为小写；本仓库为 `ghcr.io/catofes/postfixpolicy`。每个发布镜像包含 `linux/amd64` 和 `linux/arm64`，运行时自带系统 CA 和时区数据。镜像标签包含带 `v` 的版本、去掉 `v` 的版本和 `sha-<短提交号>`。最后成功发布的正式标签还会更新 `latest`，正式运行建议固定版本标签或镜像 digest。

工作流使用仓库自带的 `GITHUB_TOKEN`，无须配置 `CR_PAT`。仓库需要启用 Actions，并允许发布任务的 `contents: write` 和 `packages: write` 权限。如果 GHCR 包已存在，需要在包设置中允许本仓库 Actions 写入；首次发布后也应确认包的可见性，公开仓库不代表镜像一定允许匿名拉取。

发布步骤示例（`v1.1.0` 仅作演示，请先确定实际版本）：

```sh
# 先提交已验证的修改，再推送分支及新标签
git push origin master
git tag -a v1.1.0 -m "发布 v1.1.0"
git push origin v1.1.0
```

标签必须使用三段版本号，可带预发布后缀。若镜像构建或清单验证失败，Release 任务不会执行；修复后可在 Actions 重跑失败任务。已上传的镜像与 Release 是不同产物，应分别确认成功。

## 容器运行

使用 [compose.yaml](compose.yaml) 部署已发布镜像。准备持久化目录和配置：

```sh
mkdir -p certs queue
cp config.example.json config.json
chmod 600 config.json
```

将入站证书、私钥放到 `certs/cert.pem` 和 `certs/key.pem`，私钥限制读取权限。编辑 `config.json`：

- `ListenAddress` 改为 `0.0.0.0:2555`，供 Docker 端口映射访问；Compose 默认只向宿主机 `127.0.0.1:2555` 发布。
- `PgURL` 填写容器可访问的实际数据库地址；容器内的 `127.0.0.1` 指向容器自身。配置合适的 PostgreSQL 认证和 TLS 策略。
- `QueuePath` 保持 `/var/lib/postfix-policy/queue`，映射到宿主机 `./queue`。
- `KeyFile`、`CertFile` 保持 `/etc/postfix-policy/key.pem`、`/etc/postfix-policy/cert.pem`，并填写实际邮件主机名与路由标记。
- 如需内部 CA，将其放入 `certs` 并设置 `TLSCAFile` 为对应容器路径。

在当前目录的 `.env` 中填写实际已发布的版本，例如 `POSTFIX_POLICY_IMAGE=ghcr.io/catofes/postfixpolicy:v1.1.0`，然后运行：

```sh
docker compose config --quiet
docker compose pull
docker compose up -d
docker compose logs -f policy
```

更新时修改 `.env` 中的版本，重新执行 `pull` 和 `up -d`。队列目录必须持续保留，不能给多个实例共用，也不要放到不支持文件同步和锁的网络文件系统。镜像没有打包生产配置、私钥或队列；本地构建可使用 `docker build -t postfixpolicy:local .`，再将 `.env` 中的镜像设为该本地标签。

## 配置

完整示例见 [config.example.json](config.example.json)。时间字段的单位均为秒。

| 配置项 | 作用与默认值 |
| --- | --- |
| `ListenAddress` | 入站 SMTP 监听地址，单封邮件上限 100 MiB |
| `PgURL` | PostgreSQL 连接串，必须填写 |
| `ServerMark` / `RegionMark` / `DefaultMark` | 查询路由时允许的三个部署标记，需与数据库规则一致 |
| `Hostname` / `SmtpHostName` | 入站主机名 / 出站 EHLO 名称；后者省略时使用前者 |
| `KeyFile` / `CertFile` | 入站 STARTTLS 私钥 / 证书路径，必须填写 |
| `QueuePath` | 持久化队列目录，默认 `.`；生产环境应显式指定 |
| `SqlTimeout` | 一次路由查询的超时，默认 30 |
| `SMTPTimeout` | 一次收件人投递的超时，默认 60 |
| `ProcessCount` | 同时处理的邮件数量，默认 3 |
| `RetryCount` | 每个收件人最大投递尝试次数，含首次，默认 3 |
| `MXTLSMode` / `RelayTLSMode` | MX / 指定中继的 TLS 策略，默认 `opportunistic` / `required` |
| `TLSCAFile` | 可选的 PEM 格式额外可信 CA 路径 |

## 路由

现有数据库需要提供 `transport_senders` 表，包含 `source`、`recipient`、`region`、`destination` 列。服务不自动创建或迁移路由表。

发件人和收件人的候选顺序分别为完整邮箱、去除第一个 `+` 及其后缀的邮箱、域名、带点的父域后缀、`*`。例如 `alice+one+two@sub.example.com` 会匹配：

```text
alice+one+two@sub.example.com
alice@sub.example.com
sub.example.com
.example.com
.com
*
```

发件人在外层、收件人在内层匹配，发件人具体程度优先。相同组合只查询 `ServerMark`、`RegionMark`、`DefaultMark`，沿用数据库 `region DESC` 的排序；这不是固定的服务器优先级。

支持的出口：

- `smtp://relay.example.com:587`：指定中继，端口必须明确填写。
- `smtp://username:password@relay.example.com:587`：指定中继并使用 PLAIN 认证；凭据中的特殊字符需进行 URL 编码。
- `default://`：按 MX 优先级直投；无 MX 时尝试域名地址记录，Null MX 表示该域不接受邮件。

仅当所有候选均无规则时回退为 MX。数据库故障或错误的路由配置会保留邮件，每分钟重新查询；不会切换到另一种出口，也不消耗投递尝试次数。修复规则无需重启。

## 队列与失败处理

接收回调独立复制正文和信封，完成临时文件写入、文件同步、原子重命名及目录同步后才返回成功。保存失败返回 SMTP `451`。队列文件权限为 `0600`，正文包含中转服务器添加的投递头。

每个 `.mail` 文件包含原邮件和各收件人的 `Deliveries` 状态。后台任务逐个处理该邮件的收件人，完成一个即持久化其状态；只有所有收件人完成后才删除原文件。启动时和运行期间自动扫描队列，`ProcessCount` 限制同时处理的邮件数量。

`RetryCount` 是每个收件人的最大投递尝试次数，默认 3 次，包括首次投递。临时失败后的等待时间依次为 1、5、10、30、60 分钟，之后固定为 60 分钟；重启保留尝试次数和等待时间。正式运行应根据允许的重试窗口调整次数。

SMTP 5xx 或尝试次数耗尽时，先可靠保存一封通知，再将该收件人标记完成。通知使用空信封发件人 `MAIL FROM:<>`，收件人只来自原信封发件人，不使用邮件头替换目标。通知沿用 MX 直投方式。通知本身或空发件人邮件投递最终失败时，原文件保留，相关收件人的 `Held` 为 `true`，不再自动投递，也不生成另一封退信。

需要重新处理保留的失败通知时，先停止服务并备份文件，清除相关收件人的 `Held`、`Failed`、`LastError`，将 `Attempts` 设为 `0`、`NextAttempt` 设为零时间，再启动服务。损坏或 ID 与文件名不符的队列文件只记录错误并保留，需人工检查。崩溃留下的 `.queue-*.tmp` 不属于已确认接收的邮件，扫描器忽略它们。

这是至少一次投递：远端已接受 DATA、但本地尚未保存完成状态时崩溃，仍可能重复投递；生成退信与更新原邮件之间崩溃也可能重复通知。没有承诺跨 SMTP 和本地文件的恰好一次投递。

## TLS、超时与升级

- `MXTLSMode` 默认 `opportunistic`：MX 直投（包括退信）对端提供 STARTTLS 时使用 TLS，不以普通 CA 证书/主机名验证作为投递前提；没有 STARTTLS 时允许明文。此模式只提供机会式加密，不保证对端身份。
- `RelayTLSMode` 默认 `required`：指定 SMTP 中继必须支持 STARTTLS，并通过证书及主机名验证。可显式设为 `opportunistic` 兼容不认证的旧中继。
- 两个模式均可分别配置为 `opportunistic` 或 `required`；带用户名认证的中继始终强制 TLS 和证书验证，即使将 `RelayTLSMode` 设为 `opportunistic`，也不会在未验证的连接上发送凭据。
- TLS 最低版本仍为 1.2。机会式模式下证书不受信任也会使用加密连接；真正的 TLS 握手失败则保留并按投递重试规则处理，不自动重连明文发送。
- `TLSCAFile` 可额外信任 PEM 格式的内部 CA，供 `required` 模式使用，默认使用系统信任库。
- `SMTPTimeout` 默认 60 秒，限制一次收件人投递中的 DNS、连接、TLS 和 SMTP 操作。SIGINT/SIGTERM 会取消后台投递，队列供下次启动恢复。

旧版 `.mail` 文件仍可读取；没有收件人状态的文件按所有收件人待投递处理，旧版生成的通知会改用空信封发件人。旧版已经删除的邮件无法恢复。**升级后旧程序不理解收件人进度，回滚前须停服并备份队列，不能直接让旧版处理新队列。** 原配置新增项会使用默认值：MX 直投采用机会式 TLS，指定中继默认要求验证过的 TLS，需检查中继的证书和 STARTTLS 支持。若配置仍包含全局 `RequireTLS`，启动时会明确报错；请改用 `MXTLSMode` 和 `RelayTLSMode`。

## 验证

```sh
make check
```

测试使用本地临时目录、回环 SMTP 服务和模拟数据库，不访问业务 PostgreSQL，也不向公网投递邮件。
