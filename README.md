# rsync_tool

用 Go 编写的 SSH 双向文件同步工具。把远端代码目录同步为本地真实目录，agent 可以直接读写，修改按批次传回远端。

## 快速开始

先运行 `./scripts/build.sh` 构建单文件可执行程序 `bin/rsync_tool`（需要 Go 1.23+）。

```sh
# 首次拉取并持续双向同步；默认每 750ms 扫描、合并一次修改
./bin/rsync_tool start devbox:/home/me/project ./project

# 另一终端查看状态、结束同步
./bin/rsync_tool status ./project
./bin/rsync_tool stop ./project

# 单次双向同步，也可以用于恢复中断后的同步
./bin/rsync_tool sync devbox:/home/me/project ./project

# 冲突时，强制以指定方向为准；先备份被覆盖的目标文件
./bin/rsync_tool sync -f pull devbox:/home/me/project ./project
./bin/rsync_tool sync -f push devbox:/home/me/project ./project

# 自定义扫描间隔、忽略规则、文件大小上限
./bin/rsync_tool start --interval 300ms --exclude '*.tmp' --exclude 'build/' \
  --max-size 2097152 devbox:/home/me/project ./project
```

`start` 在前台运行，Ctrl-C 与 `stop` 都会触发最后一次同步。需要后台运行时：

```sh
nohup ./bin/rsync_tool start devbox:/home/me/project ./project >sync.log 2>&1 &
```

结束后保留本地目录及同步状态。再次 `start` 会使用上次的共同版本判断变化。一个本地目录只能绑定一个远端，同一时刻只能运行一个同步进程。

## SSH 与远端要求

本地依赖系统 `ssh`，沿用 `~/.ssh/config`、SSH agent、密钥及主机校验。支持 `devbox:/absolute/path` 和 `user@host:/absolute/path`；远端目录必须已存在。端口等参数放在 SSH config 中。

远端不需要安装 Go、Python、rsync 或本工具。默认需要 Unix shell、`uname`、`mktemp`、`cat`、`chmod` 和可执行的 `/tmp`。本地二进制内置 Linux/macOS 的 amd64/arm64 辅助程序，连接时自动上传对应版本；正常断开时清理。意外断电、SIGKILL 等情况可能留下 `/tmp/rsync_tool.*` 临时文件。

如果远端 `/tmp` 不允许执行，可以事先放置适合远端平台的 `rsync_tool`，使用：

```sh
./bin/rsync_tool start --remote-helper /home/me/bin/rsync_tool \
  devbox:/home/me/project ./project
```

无需 SSH 的本机试用：

```sh
mkdir -p /tmp/remote-demo /tmp/local-demo
./bin/rsync_tool start local:/tmp/remote-demo /tmp/local-demo
```

## 同步与冲突语义

- 首次同步合并两边的文件：远端独有文件拉取，本地独有文件上传；同名且内容不同的文件产生冲突。
- 后续同步比较两端与上次共同版本，单边修改/删除会传播；两端最终内容相同则直接确认。
- 两端同时修改，或一端删除另一端修改时，保留原位置，冲突内容保存到本地 `.rsync_tool/conflicts/`，文件名包含来源和内容哈希。删除的一端没有内容副本，状态仍记录冲突。
- 冲突不会阻塞其他文件。`sync` 遇到未解决冲突返回非零；`start` 继续监控并通过 `status` 报告。
- `-f pull` 以远端为准，`-f push` 以本地为准，对所有可同步路径生效，包括删除源端不存在的目标文件。被覆盖/删除的目标内容先备份到目标端 `.rsync_tool/conflicts/`。用于 `start` 时只作用于首次成功同步，之后恢复双向模式。
- 对写入目标再校验内容哈希，检测到扫描后的并发修改就拒绝覆盖；写入使用同目录临时文件与原子重命名。建议避免两端持续并发编辑同一个文件；这不是事务型分布式文件系统。
- 传输失败会保留已确认的共同版本；`start` 自动重连和重试，`sync` 返回失败，重新运行即可恢复。停止时若最后一次同步失败或有冲突，会返回非零并保留状态。

## 忽略与大文件

- 永远排除任意层级的 `.git`、`.rsync_tool`，以及符号链接和特殊文件。
- 读取未被忽略目录中的各层 `.gitignore`、`.rsync_toolignore`；支持通配符、目录规则、`**`、`!` 反向规则。被忽略目录不再遍历，符合 Git 无法在已排除父目录中重新包含子文件的语义。
- 同目录先应用 `.gitignore`，再应用 `.rsync_toolignore`；子目录规则覆盖父目录，重复 `--exclude` 最后应用。忽略规则文件本身也会同步，除非它被规则排除。
- 任一端规则排除的路径，都不下载、不上传、不传播删除；只在目录扫描时读取规则文件。只支持目录内的规则文件，不读取用户全局 Git ignore 或 `.git/info/exclude`。
- 默认跳过 **大于 2 MiB** 的文件；恰好 2 MiB 会同步。文件超过上限后，不会被当作删除传播。双方存在文件时，只要任一端超限就暂不处理该路径；恢复到上限内后重新参与同步。
- 同步普通文件及其权限，并支持空目录创建、目录重命名后的内容收敛，以及递归删除。目录按父子顺序创建，删除时先处理文件再处理子目录；非空目录只尝试安全移除，若内含被忽略的文件会保留目录并报告待重试，不会递归清除被忽略内容。不同步符号链接、所有者、扩展属性或 ACL，不自动处理目录与普通文件互相替换。目录权限统一使用 0755。

## 通信与实现边界

启动时用 SSH 探测平台、上传辅助程序，随后复用一条启用 SSH 压缩的连接。`start` 在两端使用文件事件通知，按 `--interval` 合并同一路径的变化，只重新扫描变化文件或子树；新增子树会补充监听，忽略规则变化会触发全量核对。每 30 秒进行一次兜底全量扫描；监听不可用、达到资源上限或发生事件溢出时，回退为每 2 秒核对。`sync` 单次命令始终进行全量扫描，`stop` 会强制全量核对后刷新，避免依赖最后一批事件是否及时送达。

未变化的远端只回复简短状态，不重复发送文件清单和忽略规则；本地跳过无变化文件的同步规划，空闲状态最多每 10 秒写一次状态文件。同一批最多 512 条路径、约 8 MiB。文件在扫描或读取期间变化时，只延后该路径，继续处理其余文件；应用错误不再导致 SSH 重连，只有通信错误才重连。

第一次或兜底扫描按修改时间、大小和权限缓存 SHA-256；收到文件变化事件时，即使元数据未变也会重新散列。仍不能保证检测既遗漏通知又刻意保留元数据的原位改写。Linux 使用 inotify，macOS 使用 kqueue；部分 macOS 目录事件可能触发较大的子树扫描，监听资源不足时会启用上述回退机制。`inspect` 输出监听错误、监听目录数、访问条目数和散列文件数，便于判断当前工作方式。

同步采用**文件级增量**：只传变化文件，变化文件发送完整内容，不实现 rsync 的块级差分。默认 2 MiB 上限限制了每次传输体积。传输通过 JSON/Base64，SSH 压缩降低文本与编码开销。

## 多仓库目录与性能诊断

如果入口目录的 `.gitignore` 将内部代码仓库整体排除，可以显式跳过入口目录的规则，同时保留子目录的忽略规则：

```sh
./bin/rsync_tool start --ignore-root-gitignore \
  devbox:/srv/workspace ./workspace

./bin/rsync_tool status ./workspace
./bin/rsync_tool stop ./workspace

# 只读扫描诊断，不同步文件、不写远端代码目录
./bin/rsync_tool inspect --samples 10 --ignore-root-gitignore \
  devbox:/srv/workspace

# 对比全量扫描（不启用事件监听）
./bin/rsync_tool inspect --watch=false --samples 5 devbox:/srv/workspace/project
```

`--ignore-root-gitignore` 只跳过入口目录的 `.gitignore`，不会跳过入口 `.rsync_toolignore`、子目录规则、`.git` 排除或文件大小限制。`start`/`sync` 的参数会同时应用于两端；重新启动时请保持相同选项。

约 4,000 个文件的实际 SSH 测试中，优化前空闲检查中位数约 1.53 秒，启用监听后约 87 毫秒，后者主要受网络往返影响。无变更时访问条目与重新散列文件均为 0。2,000 个小文件的递归删除约需 1–1.5 秒，连续 100 次、间隔 5ms 的改写约 1 秒收敛。这些结果用于展示工作方式，具体表现取决于硬件、网络和目录结构。

首次连接仍需要 SSH 建连和上传辅助程序，首次拉取仍会传输全部符合规则的内容。优化主要降低常驻同步阶段的空闲和小批量变更成本，尚未实现块级差分。

## 构建与验证

需要 Go 1.23+，首次构建需要下载 Go 模块。

```sh
./scripts/build.sh
# 输出 bin/rsync_tool，并生成四个平台的嵌入式辅助程序

go test ./...
go vet ./...
python3 scripts/integration_test.py
# 可选：真实 SSH 压测，仅创建并清理自己的 /tmp 临时目录
python3 scripts/ssh_benchmark.py devbox --files 2000
```

可通过 `GOOS=linux GOARCH=amd64 ./scripts/build.sh` 构建其他本地目标；该脚本仍生成全部远端辅助程序。

单元测试覆盖双向修改、删除、内容/删除冲突、强制覆盖备份、嵌套忽略与反向规则、大小边界、符号链接、并发覆盖保护、批量 RPC 和清单增量。集成测试使用本机 SSH 替身，验证上传/清理辅助程序、命令转义、持久连接、持续同步、停止前刷新和状态；不验证真实 SSH 服务器的认证及网络故障。另有 `ssh_benchmark.py` 已通过真实 SSH 验证，覆盖大量文件、连续改写、递归删除、空目录、远端创建及停止前刷新；尚未注入网络中断。
