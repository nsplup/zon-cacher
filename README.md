*本项目与 README 由 `Gemini` 完成，`我` 监制*

# zon-cacher 🚀

`zon-cacher` 是一个用 Go 语言编写的命令行工具，专门用于**预下载并本地缓存 Zig 语言项目的 `build.zig.zon` 依赖项**。

在网络环境较差、离线开发，或需要加速 CI/CD 构建的场景下，该工具可以帮助你提前把所有远程依赖项（如 GitHub 压缩包、Git 仓库）拉取到 Zig 的全局缓存目录中，从而避免 `zig build` 时因网络问题导致的下载失败。

---

## ✨ 核心特性

* **递归解析**：深度解析目标目录下的 `build.zig.zon` 文件，支持多层嵌套的本地路径依赖（`.path`）。
* **多协议支持**：
* **HTTP/HTTPS 压缩包**：支持 `.zip`、`.tar.gz`、`.tar.xz`、`.tar.zst` 等常见格式的自动下载与解压。
* **Git 仓库**：支持智能识别 `git+`、`git://` 或带有特定 Commit 锚点的纯 Git 依赖，并进行浅克隆（`--depth 1`）优化。


* **跨平台支持**：自动适配 Windows、macOS 和 Linux 的 Zig 全局缓存路径。
* **安全清洗**：内置 ZON 注释清洗器，解析时会自动剥离 `//` 单行注释，防止正则匹配失效。
* **智能去重（Cache Hit）**：下载前校验 Hash，若本地缓存已存在则直接跳过并递归，绝不重复下载。

---

## 🛠️ 前置要求

使用本工具前，请确保系统中已安装以下命令行工具：

* **Go** (用于编译本工具，推荐 1.16+)
* **Git** (处理 Git 依赖时需要)
* **tar** 或 **unzip** (解压下载的压缩包时需要)

---

## 📦 安装方法

你可以克隆代码并直接编译为二进制文件：

```bash
# 下载依赖
go mod tidy

# 编译成二进制文件
go build -o zon-cacher

```

---

## 🚀 使用指南

### 1. 基础用法

传入你希望解析的 Zig 项目根目录（包含 `build.zig.zon` 的目录）：

```bash
zon-cacher <目标项目目录路径>

```

**示例：**

```bash
zon-cacher ./my-zig-project

```

### 2. 运行效果示例

```text
🚀 开始解析目录: /Users/username/workspace/my-zig-project
📦 正在解析: /Users/username/workspace/my-zig-project/build.zig.zon
✅ 依赖已存在 (Cache Hit): zig-zap -> 120fc...
🚚 正在下载依赖 [mimalloc]...
💾 依赖已成功缓存: /Users/username/.cache/zig/p/120f...
✨ 所有依赖处理完毕！

```

---

## 🔍 工作原理

1. **定位路径**：读取输入的目录，寻找 `build.zig.zon`。
2. **清洗与正则提取**：剥离文件中的所有 `//` 释，利用严格的正则表达式提取出 `.dependencies` 代码块中的 `url`、`hash` 或 `path`。
3. **计算全局缓存（Global Cache）**：
* **Windows**: `%LocalAppData%\zig`
* **Linux/macOS**: 首选 `$XDG_CACHE_HOME/zig`，次选 `$HOME/.cache/zig`


4. **下载与解压**：将依赖下载到临时目录，解压后重命名移动至全局缓存的 `p/<HASH>` 目录下。
5. **循环迭代**：如果该依赖内部还包含 `build.zig.zon`，则会以此类推递归处理。