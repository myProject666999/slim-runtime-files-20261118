# `pkg/fileset` — 运行期最小文件集合成设计说明

## 1. 目的与模型

运行期监控（fanotify/ptrace）只告诉我们“某个路径被访问过”。但观察到一个共享库**符号链接**
被打开，并不等于只保留该链接即可：真实文件可能在链接链的另一端，而链上的每一级父目录都必须可进入。
`fileset` 从可能数十万条、大量重复的访问事件中，合成一个去重、自洽、可重建的最小文件集。

一个 `Builder` 绑定到一个**已从镜像解包到本地的容器根目录 `rootDir`**。事件里的路径恒为**容器内绝对
路径**（以 `/` 开头）。本包绝不把容器路径放到宿主根下解释：任何检查路径都是
`filepath.Join(rootDir, 容器路径)`，且每次拼接都做逃逸围栏。

### 只读契约

全包对文件系统只用 `os.Lstat` 与 `os.Readlink`，且都作用于 `rootDir` 之内。**不使用**
`Stat`（跟随链接）、`filepath.EvalSymlinks`、`ReadDir`、`Walk`，因此：

- 永远不会把一个目录的内容递归纳入集合；
- 永远不会修改或复制任何文件；
- 不会以宿主根为基准解析符号链接。

`Builder` 非并发安全；事件扇入应在调用 `Add` 前同步到单 goroutine，从而让热路径无锁。

## 2. 处理流水线

```
事件 (containerPath, op)
  └─ normalize：filepath.Clean 折叠 //、/./、词法 ..（根处钳制）；非绝对路径拒绝
       └─ classify：伪文件系统可审计规则表（见 §5），过滤项直接计入 PseudoFiltered
            └─ walk：内核式逐组件解析，记录每个名称（祖先目录 / 中间符号链接 / 终态对象）
                 └─ applyObserved：把本次访问强度合并到“被观察名 + 终态对象”
Build：map 快照 + 字典序 Sorted + 计数快照
```

### 为什么必须逐组件解析

`Lstat` 只对路径的**最后一个组件**不跟随链接，对中间组件仍会跟随。因此对
`/srv/current/cfg`（`current -> /srv/v1`）直接 `Lstat` 整条路径，返回的是 `cfg` 真实文件的属性，
**永远发现不了 `current` 是符号链接**（已用可执行示例实证）。本包改为一次消费一个组件、对每个组件
单独 `Lstat`，从而暴露并记录链上的每个链接；同一循环也顺带记录了每一级父目录。

### 容器命名空间内的链接目标重锚

- 相对目标：以**链接所在目录**为基准 —— `path.Join(path.Dir(link), target)`。
- 绝对目标：以**容器根 `/`** 为基准 —— `path.Join("/", target)`，绝不像宿主代码
  `dockerimage.linkTargetToFullPath` 那样把绝对目标原样返回（那会泄漏到宿主文件系统）。

### 成环、悬空与跳数上限

- 成环：维护已访问符号链接集合，下一个目标已在集合中即判定成环（含 `a -> a` 自环）。告警、
  `Stats.Loops++`、仅终止本次 walk；环上名称全部保留，不影响其他事件与整体 `Build`。
- 跳数上限：默认 `maxHops=40`（对齐 Linux `MAXSYMLINKS`/`ELOOP`）。独立于 visited 集合，兜住病态长链。
- 悬空：链接本体 `Lstat` 成功（记录名称与目标字符串），下一名称 `ENOENT`。从首个缺失处到完整目标名
  之间的整条缺失后缀都记录为 `KindMissing`（保证“链上所有名称入集”），告警同时挂到触发链接与缺失终端，
  `Stats.Dangling++` 后停止。
- 普通缺失（未经过任何符号链接）：记录为 `KindMissing`，`Stats.Missing++`。

### 指向目录的符号链接不递归

walk 只记录被解析到的名称，从不列目录。`/logs -> /var/log` 上访问 `/logs/app.log` 只会得到
`/logs`（链接）、`/var/log`（目录）、`/var/log/app.log`（文件）；`/var/log` 的其它子项不会进入集合。

## 3. 去重与访问语义（内存有界）

单一后端 `map[string]*Entry`，键是规范化容器路径。父目录、链接链名、重复事件都汇聚到同一槽位，
map 规模由**唯一路径数**界定，与事件总量无关。

- 已完成 walk 的观察路径记入 `done`，重复事件不再触发任何系统调用、也不会重复累加异常计数。
- 被过滤路径记入 `filtered`，重复仅 `O(1)` 累加 `PseudoFiltered`。
- 访问强度按秩合并：`Check(1) < Read(2) < Exec(3) < Write(4)`。同一路径先读后写，最终为 `Write`，
  作为后续是否保留写权限的依据。强度同时合并到“被观察名”与其链终态对象；结构性的祖先目录与中间
  符号链接保持 `Check`。benchmark 实测重复 `Add` 为 **0 分配/次**。

## 4. 元数据

每个名称的属性都来自**该名称自身的 `Lstat`**（符号链接取链接自身而非目标）：

| 字段 | 来源 |
|---|---|
| `Mode` | `FileInfo.Mode()`；权限位取 `.Perm()`，类型位驱动 `Kind` |
| `Uid/Gid/Inode` | 受保护断言 `fi.Sys().(*syscall.Stat_t)`；`Sys()==nil` 时 Uid/Gid 置 -1 |
| `Mtime` | `Stat_t.Mtim`（Linux）/ `FileInfo.ModTime()`（可移植回退） |
| `Ctime` | `Stat_t.Ctim`（仅 Linux） |
| `Size` | `FileInfo.Size()` |

刻意**不采集 atime**：任何读取（包括第二次合成时我们自己的 `Lstat`）都会改写 atime，使其跨运行
不确定，无法用于确定性断言。平台差异通过 `meta_linux.go` / `meta_other.go` 的文件名隐式构建约束隔离，
主包无 build tag，在任意平台都能编译。

## 5. 伪文件系统可审计规则表

规则以有序数据表实现（`pseudo.go` 的 `classify`），每条规则有稳定的 `Reason` 常量供测试与审阅断言。

| # | 路径类 | 动作 | 理由 / 取舍 |
|---|---|---|---|
| 1 | `/` 本身 | **保留** | 镜像根是真实 tar 条目，是父目录元数据的顶点（区别于 `artifact.IsFilteredPath("/")==true` 的丢弃行为） |
| 2 | `/proc` `/sys` `/dev` 挂载点目录 | **保留** | 镜像内预置的空挂载点目录；保留以为例外子项提供祖先，否则保留的 `/dev/...` 节点会成为孤儿 |
| 3 | 任意 `/proc/...` 非符号链接 | 过滤 | 内核按 pid 生成的文本/目录，非镜像内容、跨运行不确定 |
| 4 | `/proc/...` 符号链接且目标解析到**镜像内已存在**路径 | **保留（例外 b）** | 如 `/proc/<pid>/fd/N -> /var/log/app.log`，是进程真实使用镜像文件的证据；目标链照常纳入 |
| 5 | `/proc/...` 链接目标为内核对象或镜像外（`socket:[…]`/`pipe:[…]`/`anon_inode:[…]`/不存在） | 过滤 | 仅内核对象，镜像中无文件可保 |
| 6 | `/proc/...` 链接目标重新落入 `/proc` 或 `/sys` | 过滤 | 对目标重新过表，防止 proc→proc/sys 链接绕过过滤 |
| 7 | 任意 `/sys/...`（含符号链接） | 过滤 | sysfs 内核所有，实践不指向镜像载荷；与 /proc 的不对称在此显式说明 |
| 8 | `/dev/...` 在 `rootDir` 下**物理存在**（`Lstat` 成功） | **保留（例外 a）** | 镜像层预置的节点/静态文件（如 `/dev/null`、`/dev/urandom`），保留真实 mode/属主/时间 |
| 9 | `/dev/...` 在 `rootDir` 下不存在 | 过滤 | 运行期 tmpfs/伪设备，镜像中无可复制项 |
| 10 | 其他 | 保留 | 普通镜像内容 |

### 两个例外的判别机制

- **例外 (a) 镜像预置 /dev 节点**：仅以解包根内 `Lstat` 是否成功判别。测试中 `rootDir` 即镜像物化，
  “在镜像层里”等价于“`rootDir` 下存在”。运行期伪设备没有对应宿主路径，`Lstat` 失败即过滤。无需挂载表或
  运行期知识。`mknod` 字符节点的测试在无特权环境下 `t.Skip`，存在性逻辑已由普通文件用例无特权覆盖。
- **例外 (b) /proc 指向镜像内的符号链接**：先确认是符号链接；`Readlink` 后先识别 `socket:`/`pipe:`/
  `anon_inode:` 等非路径内核对象（直接过滤），再用同一套容器根重锚得到目标；若目标仍在 `/proc`/`/sys`
  则重新过表过滤，否则对目标 `Lstat` 成功才保留并继续 walk。

`WithPseudoExceptions(false)` 可关闭两个例外，退化为对 `/proc /sys /dev` 内容的全量过滤（挂载点目录仍保留）。

## 6. 关键边界情形

- 根 `/`：始终作为带元数据的目录记录，父链在此终止。
- `..` 逃逸：`normalize` 词法钳制 + `hostOf` 的 `filepath.Rel` 运行期围栏，双重防止越出 `rootDir`。
- 硬链接：不同路径名分别入集（每个被观察的名字都要存活）；共享 `Stat_t.Ino` 仅暴露在 `Meta.Inode`，
  供下游重建，本包不做 inode 去重。
- 特殊文件：fifo/socket/字符/块设备是 Lstat 推导的终态 `Kind`，不读内容。
- 未知 op：降级为 `Check` 并告警，不丢弃、不报错中断。
- 事件顺序：所有状态在单 map 内按路径汇聚、`Build` 排序，结果与顺序无关（压力测试以 10 万乱序事件证明）。
- 多组件后缀（`/link/dir/file`）：由逐组件算法天然处理，不做字符串前缀 hack。
- Lstat 权限失败：保留名称、元数据零值、告警并终止该 walk，单路径失败不拖垮整次采集。

## 7. 测试与验证

全部为 OS 无关、无 build tag 的普通单元测试，`go test ./...` 即可运行，无需 root/Docker。

- `normalize_test.go`：规范化、目标重锚、组件拆分、访问秩合并、根逃逸围栏。
- `chain_test.go`：在 `t.TempDir()` 构造两级链/相对链/绝对链/环/自环/悬空链/目录链，断言集合、`Kind`、
  `LinkTarget`、权限位、mtime、uid/gid/inode，以及目录链不纳入兄弟文件。
- `pseudo_test.go`：普通 `/proc /sys /dev` 项过滤；例外 (a)(b) 保留与理由；内核对象与 proc→sys 重分类过滤；
  关闭例外的全量过滤。
- `stress_test.go`：固定种子生成 10 万条乱序、op 轮转、重复且穿插伪 fs 的事件，断言与去重单次基线
  `Entries/Sorted` 完全一致；10 万次 `< 300ms`；重复批次堆增长 `< 2MiB`；已存在路径 `AllocsPerRun==0`；
  附 `BenchmarkAdd`（实测约 78 ns/op、0 B/op、0 allocs/op）。
