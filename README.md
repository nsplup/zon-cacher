*本项目与 README 由 `Deepseek` 与 `GLM` 共同完成，`我` 监制*

# zon-cacher
**Zig 依赖离线缓存工具** —— 预先把 `build.zig.zon` 中的全部依赖抓取到 Zig 全局缓存与项目本地 `zig-pkg/` 目录，使 `zig build` 可以完全离线执行，并支持加速 CI、代理环境与重复构建。
---
## 目录
- [背景](#背景)
- [特性](#特性)
- [安装](#安装)
- [使用](#使用)
- [命令行参数](#命令行参数)
- [工作原理](#工作原理)
- [缓存布局](#缓存布局)
- [典型场景](#典型场景)
- [常见问题](#常见问题)
- [系统要求](#系统要求)
---
## 背景
从 **Zig 0.16** 起（[zig#31121](https://github.com/ziglang/zig/pull/31121)），包管理器的缓存契约发生了变化：
1. 依赖被解包到 `<构建根>/zig-pkg/<name>-<version>-<hash>/`，且目录内容必须是**应用了 `build.zig.zon` 中 `.paths` 过滤之后的树**（包 hash 正是按这棵过滤后的树计算的）；
2. 全局缓存 `$GLOBAL_ZIG_CACHE/p/$HASH.tar.gz` 是 Zig 自己抓取并重新压缩出的**规范（canonical）tarball**，用作离线时的网络替代品。
如果手工填充缓存时不符合该契约（例如未应用 `paths` 过滤、tarball 格式不规范），`zig build` 的 hash 校验会失败，依赖被视作“未获取”，进而出现类似下面的错误：
```
panic: unable to find module "aro"
```
`zon-cacher` 严格按 Zig 0.16 的契约生成缓存，避免上述问题。
## 特性
- ✅ **规范 blob 缓存**：为每个依赖生成确定性（deterministic）tar.gz —— 条目按字典序排序、无 `./` 前缀、mtime/uid/gid 归零、gzip 固定压缩级别，与 Zig 重新打包的规范格式对齐；
- ✅ **`.paths` 过滤**：解析包自身 `build.zig.zon` 中的 `.paths` 白名单（缺失时回退到依赖条目声明的 `.paths`），构建缓存前剔除白名单之外的文件，保证解包树 hash 与声明 hash 一致；
- ✅ **本地 `zig-pkg/` 填充**：把依赖解包到项目本地 `zig-pkg/`，供 `zig build` 直接使用；
- ✅ **传递依赖解析**：递归扫描依赖自身的 `build.zig.zon`，一并缓存子依赖（支持 `.path` 本地依赖）；
- ✅ **Git 依赖支持**：`git+https://...#<commit>` 形式的依赖通过浅克隆 + checkout 指定 commit 获取；
- ✅ **多格式归档**：`.tar.gz` / `.tgz` / `.tar.xz` / `.tar.zst` / `.zip`；
- ✅ **Legacy 模式**：兼容 Zig ≤ 0.15 的“解包目录”缓存布局；
- ✅ **缓存自愈**：blob 带格式版本标记，旧版本/非规范缓存自动重建；
- ✅ **跨设备安全**：所有临时文件创建在与目标相同的文件系统内，避免跨设备 `rename` 失败；
- ✅ **代理支持**：HTTP 下载遵循 `HTTP_PROXY` / `HTTPS_PROXY` 环境变量，git 请求自动注入 `https.proxy` 配置；
- ✅ **重试机制**：HTTP 下载失败自动重试 3 次。
## 安装
```bash
git clone <本仓库>
cd zon-cacher
go build -o zon-cacher .
```
将生成的 `zon-cacher` 放入 `PATH` 或直接在原地使用。
## 使用
```bash
# 基本用法：为项目预取全部依赖
zon-cacher /path/to/your/zig/project
# 之后即可离线构建
cd /path/to/your/zig/project
zig build -Doptimize=ReleaseFast
```
### 命令行参数
```
使用方法: zon-cacher [-legacy] [-local-pkg=true|false] <项目目录>
  -legacy
        旧版解压模式 (Zig <= 0.15, 全局缓存为解包目录)
  -local-pkg
        填充项目本地 zig-pkg/ 目录 (默认 true)
```
| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-legacy` | `false` | 使用 Zig ≤ 0.15 的缓存布局（全局缓存中直接存放解包后的目录，而非 tarball） |
| `-local-pkg` | `true` | 是否把依赖解包到项目本地 `zig-pkg/` 目录 |
## 工作原理
```
项目 build.zig.zon
        │
        ▼
┌─────────────────────────────┐
│ 1. 解析 .dependencies       │  (url / hash / path / paths)
└─────────────────────────────┘
        │
        ▼
┌─────────────────────────────┐
│ 2. 获取源码树               │  git clone / 下载归档并解压
└─────────────────────────────┘
        │
        ▼
┌─────────────────────────────┐
│ 3. 应用 .paths 过滤         │  剔除白名单之外的文件
│    (build.zig/.zon 始终保留)│
└─────────────────────────────┘
        │
        ├──────────────────────────────┐
        ▼                              ▼
┌───────────────────────┐   ┌────────────────────────┐
│ 4a. 规范 tar.gz       │   │ 4b. 解包到项目         │
│ → ~/.cache/zig/p/     │   │     zig-pkg/<hash>/    │
│    <hash>.tar.gz      │   │    (重建而非复用)      │
└───────────────────────┘   └────────────────────────┘
        │
        ▼
┌─────────────────────────────┐
│ 5. 递归扫描依赖自身的       │
│    build.zig.zon (传递依赖) │
└─────────────────────────────┘
```
### 关键设计点
1. **规范 tarball**：使用 Go 标准库 `archive/tar` + `compress/gzip` 生成，保证：
   - 文件/目录条目按字典序排序；
   - 路径不含 `./` 前缀；
   - `mtime`、`uid`、`gid`、`uname`、`gname` 全部归零；
   - 权限归一为 `0644` / `0755`；
   - gzip 使用固定压缩级别，输出字节级确定。
2. **本地 `zig-pkg/` 始终从 blob 重建**：只有当本地目录的 mtime 比对应 blob **新**时（说明确实由本工具从该 blob 解出）才复用，否则删除重建，杜绝旧版工具或 Zig 自己留下的不一致目录。
3. **缓存版本标记**：每个 blob 旁写入 `<hash>.tar.gz.zon-cacher-meta` 标记文件。标记不存在或版本不匹配时，blob 会被自动重建 —— 升级本工具后无需手动清缓存。
## 缓存布局
### Blob 模式（Zig ≥ 0.16，默认）
```
~/.cache/zig/p/                          # 全局缓存（XDG_CACHE_HOME 可覆盖前缀）
├── <hash>.tar.gz                        # 规范 tarball（应用 paths 过滤后的树）
├── <hash>.tar.gz.zon-cacher-meta        # 格式版本标记
└── ...
<project>/zig-pkg/                       # 项目本地
├── translate_c-2.0.0-Q_BUWltOBwB.../    # 解包后的依赖源码树
├── aro-0.0.0-JSD1QtuBNwCA.../
└── ...
```
### Legacy 模式（Zig ≤ 0.15，`-legacy`）
```
~/.cache/zig/p/
├── <hash>/                              # 解包后的依赖源码树
├── <hash>.zon-cacher-complete           # 完成标记（位于包目录外，不污染内容）
└── ...
```
> Legacy 模式下完成标记放在包目录**外部**，避免改变包内容影响 hash 校验。
## 典型场景
### 离线构建 / 沙箱编译
```bash
# 联网机器上预取
zon-cacher ~/Desktop/ghostty-1.4.0
# 拷贝整个项目目录（含 zig-pkg/）到离线机器
rsync -a ~/Desktop/ghostty-1.4.0 offline-host:~/build/
# 离线机器上直接构建
cd ~/build/ghostty-1.4.0 && zig build
```
### CI 缓存加速
把 `~/.cache/zig/p/`（或项目内 `zig-pkg/`）作为 CI 缓存目录。首次运行 `zon-cacher` 后，后续构建无需访问网络。
### 通过代理下载
```bash
export HTTPS_PROXY=http://proxy:8080
zon-cacher /path/to/project
```
HTTP 下载与 `git` 操作均会遵循代理设置。
## 常见问题
### Q: 已经跑过旧版本 zon-cacher，需要清缓存吗？
不需要。旧版本生成的 blob 没有格式版本标记，新版本会检测到并**自动重建**。项目本地 `zig-pkg/` 也会因 mtime 判断而自动刷新。如果想彻底重来：
```bash
rm -rf ~/.cache/zig/p <project>/zig-pkg
zon-cacher <project>
```
### Q: 报 `unable to find module "xxx"` panic？
通常是缓存内容与 `build.zig.zon` 声明的 hash 不一致（依赖被 Zig 视作未获取）。请确保：
1. 使用**当前版本**的 zon-cacher（应用了 `.paths` 过滤与规范打包）；
2. 清空 `~/.cache/zig/p` 与项目 `zig-pkg/` 后重新运行；
3. `zig build` 时不要额外传 `-Dfetch` 之类的网络参数。
### Q: 某个依赖下载失败 / git clone 超时？
- 检查网络与代理设置（`HTTP_PROXY` / `HTTPS_PROXY`）；
- HTTP 下载内置 3 次重试；git 浅克隆失败会自动回退为完整 fetch；
- 删除对应的 `.tar.gz`（及 `.zon-cacher-meta`）后重跑即可单点重试。
### Q: `.tar.zst` 依赖如何处理？
自动支持。解压优先使用 `tar --zstd`，不可用时回退为 `zstd -d` + `tar -x` 两步。
### Q: 支持哪些 Zig 版本？
- **Zig ≥ 0.16**：默认 Blob 模式；
- **Zig ≤ 0.15**：使用 `-legacy` 标志。
## 系统要求
| 组件 | 要求 |
|------|------|
| Go | ≥ 1.21（构建用） |
| tar | 系统命令（解压归档） |
| git | 系统命令（git 依赖） |
| unzip | 可选（`.zip` 依赖） |
| zstd | 可选（`.tar.zst` 依赖的回退解压） |
| 平台 | Linux / macOS / Windows（Windows 使用 `%LocalAppData%\zig` 作为缓存根） |
---
## License
MIT
