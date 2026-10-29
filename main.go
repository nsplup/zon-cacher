package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// cacheFormatVersion: 缓存格式版本。升级过滤/打包逻辑时递增，
// 旧版本的缓存（meta 标记不匹配）会被自动重建。
const cacheFormatVersion = "3"

type Dependency struct {
	Name  string
	URL   string
	Hash  string
	Path  string
	Paths []string // build.zig.zon 中声明的 .paths 过滤白名单
}

type Mode int

const (
	ModeBlob Mode = iota
	ModeLegacy
)

type Config struct {
	Mode        Mode
	CacheRoot   string
	LocalPkgDir string
	LocalPkg    bool
	Visited     map[string]bool
}

var httpClient = &http.Client{
	Timeout: 10 * time.Minute,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	},
}

func main() {
	legacy := flag.Bool("legacy", false, "旧版解压模式 (Zig <= 0.15, 全局缓存为解包目录)")
	localPkg := flag.Bool("local-pkg", true, "填充项目本地 zig-pkg/ 目录 (默认 true)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "使用方法: zon-cacher [-legacy] [-local-pkg=true|false] <项目目录>\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		flag.Usage()
		os.Exit(1)
	}

	projectDir, err := filepath.Abs(args[0])
	if err != nil {
		fmt.Printf("❌ 无法获取绝对路径: %v\n", err)
		os.Exit(1)
	}

	mode := ModeBlob
	if *legacy {
		mode = ModeLegacy
	}

	cfg := &Config{
		Mode:        mode,
		CacheRoot:   filepath.Join(getGlobalCacheDir(), "p"),
		LocalPkgDir: filepath.Join(projectDir, "zig-pkg"),
		LocalPkg:    *localPkg && !*legacy,
		Visited:     make(map[string]bool),
	}

	printBanner(cfg, projectDir)

	if err := processDirectory(projectDir, cfg); err != nil {
		fmt.Printf("❌ 出错: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✨ 所有依赖处理完毕！")
}

func printBanner(cfg *Config, projectDir string) {
	switch cfg.Mode {
	case ModeBlob:
		fmt.Println("📌 模式: Blob (Zig >= 0.16, 规范 tar.gz + paths 过滤)")
	case ModeLegacy:
		fmt.Println("📌 模式: Legacy (Zig <= 0.15)")
	}
	if cfg.LocalPkg {
		fmt.Println("📌 填充 zig-pkg/: 已启用")
	}
	fmt.Printf("📌 缓存格式版本: v%s\n", cacheFormatVersion)
	fmt.Printf("📂 全局缓存: %s\n", cfg.CacheRoot)
	fmt.Printf("📂 本地 zig-pkg: %s\n", cfg.LocalPkgDir)
	fmt.Printf("🚀 解析目录: %s\n", projectDir)
}

func processDirectory(dir string, cfg *Config) error {
	zonPath := filepath.Join(dir, "build.zig.zon")
	if _, err := os.Stat(zonPath); os.IsNotExist(err) {
		return nil
	}
	content, err := os.ReadFile(zonPath)
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", zonPath, err)
	}
	deps := parseZON(string(content))
	if len(deps) == 0 {
		return nil
	}

	for _, dep := range deps {
		if dep.Path != "" {
			localAbs := filepath.Clean(filepath.Join(dir, dep.Path))
			if err := processDirectory(localAbs, cfg); err != nil {
				return err
			}
			continue
		}
		if dep.Hash == "" {
			continue
		}
		if cfg.Visited[dep.Hash] {
			continue
		}
		cfg.Visited[dep.Hash] = true

		if cfg.Mode == ModeBlob {
			if err := handleBlob(dep, cfg); err != nil {
				return err
			}
		} else {
			if err := handleLegacy(dep, cfg); err != nil {
				return err
			}
		}
	}
	return nil
}

// ============================================================================
// Blob 模式 (Zig >= 0.16)
//
// Zig 0.16 的契约 (PR #31121):
//   1) 包解包到 <构建根>/zig-pkg/<name>-<version>-<hash>/，内容必须是应用了
//      build.zig.zon 中 .paths 过滤之后的树（hash 按这棵树计算）；
//   2) 全局缓存 $GLOBAL_ZIG_CACHE/p/$HASH.tar.gz 是 zig 打包的"规范" tarball，
//      用作离线时的网络替代品。
// 因此: blob 必须由过滤后的树生成，且 zig-pkg 目录必须始终从规范 blob 重建。
// ============================================================================

func handleBlob(dep Dependency, cfg *Config) error {
	blobPath, err := ensureGlobalBlob(dep, cfg)
	if err != nil {
		return err
	}

	if cfg.LocalPkg {
		pkgDir := filepath.Join(cfg.LocalPkgDir, dep.Hash)
		bfi, err := os.Stat(blobPath)
		if err != nil {
			return err
		}
		// 只有当本地目录比 blob 新（说明确实是本工具从该 blob 解出来的）才复用；
		// 否则一律从规范 blob 重建，杜绝旧版工具留下的不一致目录。
		if fi, err := os.Stat(pkgDir); err == nil && fi.ModTime().After(bfi.ModTime()) {
			fmt.Printf("✅ zig-pkg 已存在: %s\n", dep.Hash)
			return processDirectory(pkgDir, cfg)
		}
		_ = os.RemoveAll(pkgDir)
		fmt.Printf("📦 填充 zig-pkg: %s (%s)\n", dep.Name, dep.Hash)
		if err := extractBlobToDir(blobPath, pkgDir); err != nil {
			return fmt.Errorf("填充 zig-pkg 失败 [%s]: %w", dep.Name, err)
		}
		return processDirectory(pkgDir, cfg)
	}

	// 未启用本地 zig-pkg：也要解析传递依赖，解到临时目录递归处理
	tmpDir, err := os.MkdirTemp("", "zon-scan-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	scanDir := filepath.Join(tmpDir, "pkg")
	if err := extractBlobToDir(blobPath, scanDir); err != nil {
		return err
	}
	return processDirectory(scanDir, cfg)
}

// ensureGlobalBlob 确保 $CACHE/p/<hash>.tar.gz 存在且为"规范"格式
// （应用 paths 过滤 + 确定性 tar/gzip）。带 meta 标记的 blob 直接命中；
// 无标记（旧版工具或需要重建）的 blob 自动重建。
func ensureGlobalBlob(dep Dependency, cfg *Config) (string, error) {
	blobPath := filepath.Join(cfg.CacheRoot, dep.Hash+".tar.gz")
	metaPath := blobPath + ".zon-cacher-meta"

	if data, err := os.ReadFile(metaPath); err == nil &&
		strings.TrimSpace(string(data)) == "zon-cacher-v"+cacheFormatVersion {
		if _, err := os.Stat(blobPath); err == nil {
			return blobPath, nil
		}
	}
	if _, err := os.Stat(blobPath); err == nil {
		fmt.Printf("♻️  重建非规范缓存 blob: %s.tar.gz\n", dep.Hash)
		os.Remove(blobPath)
	}

	fmt.Printf("🚚 准备全局缓存: %s -> %s.tar.gz\n", dep.Name, dep.Hash)
	if err := os.MkdirAll(cfg.CacheRoot, 0755); err != nil {
		return "", err
	}

	// 1) 获取源码树并应用 paths 过滤
	treeDir, cleanup, err := buildPreparedTree(dep, cfg)
	if err != nil {
		return "", err
	}
	defer cleanup()

	// 2) 打包为规范 tar.gz（tmp 建在 cacheRoot 内，确保 rename 同 fs）
	tmpFile, err := os.CreateTemp(cfg.CacheRoot, ".tmp-blob-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	if err := writeCanonicalTarGz(treeDir, tmpPath); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, blobPath); err != nil {
		return "", err
	}
	_ = os.WriteFile(metaPath, []byte("zon-cacher-v"+cacheFormatVersion+"\n"), 0644)
	fmt.Printf("💾 全局缓存完成: %s.tar.gz (规范格式, 已应用 paths 过滤)\n", dep.Hash)
	return blobPath, nil
}

// buildPreparedTree 下载/克隆依赖，返回应用 paths 过滤后的源码树目录。
// 返回 cleanup 用于清理临时目录。
func buildPreparedTree(dep Dependency, cfg *Config) (string, func(), error) {
	tmpRoot, err := os.MkdirTemp("", "zon-prep-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmpRoot) }

	inner := filepath.Join(tmpRoot, "inner")
	if err := os.MkdirAll(inner, 0755); err != nil {
		cleanup()
		return "", nil, err
	}

	var treeDir string
	if isGitURL(dep.URL) {
		if err := downloadGit(dep.URL, inner); err != nil {
			cleanup()
			return "", nil, err
		}
		treeDir = inner // Git 依赖不做 determineSourceDir（避免误判丢一层）
	} else {
		if err := downloadAndExtractArchive(dep.URL, inner); err != nil {
			cleanup()
			return "", nil, err
		}
		treeDir = determineSourceDir(inner)
	}

	// 关键修复: 应用 paths 过滤。
	// 优先使用包自身 build.zig.zon 中的 .paths（zig 即按此计算 hash）；
	// 包自身没有声明（naked package）时，退回依赖条目里声明的 .paths。
	paths := readPackagePaths(treeDir)
	if len(paths) == 0 {
		paths = dep.Paths
	}
	if err := filterTree(treeDir, paths); err != nil {
		cleanup()
		return "", nil, err
	}
	return treeDir, cleanup, nil
}

func extractBlobToDir(blobPath, destDir string) error {
	parent := filepath.Dir(destDir)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	// tmp 建在目标父目录内，避免跨设备 rename
	tmpDir, err := os.MkdirTemp(parent, ".tmp-extract-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	cmd := exec.Command("tar", "-xzf", blobPath, "-C", tmpDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("解压 blob 失败: %w", err)
	}
	return movePath(tmpDir, destDir)
}

// ============================================================================
// Legacy 模式 (Zig <= 0.15)
// ============================================================================

func handleLegacy(dep Dependency, cfg *Config) error {
	dirPath := filepath.Join(cfg.CacheRoot, dep.Hash)
	// 修复: marker 放在包目录外，避免污染包内容影响 hash 校验
	markerPath := filepath.Join(cfg.CacheRoot, dep.Hash+".zon-cacher-complete")

	if data, err := os.ReadFile(markerPath); err == nil &&
		strings.TrimSpace(string(data)) == "zon-cacher-v"+cacheFormatVersion+"-legacy" {
		fmt.Printf("✅ Legacy 缓存命中: %s\n", dep.Hash)
		return processDirectory(dirPath, cfg)
	}

	if _, err := os.Stat(dirPath); err == nil {
		os.RemoveAll(dirPath)
	}

	fmt.Printf("🚚 Legacy 下载: %s -> %s\n", dep.Name, dep.Hash)
	if err := os.MkdirAll(cfg.CacheRoot, 0755); err != nil {
		return err
	}

	treeDir, cleanup, err := buildPreparedTree(dep, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := movePath(treeDir, dirPath); err != nil {
		return err
	}
	if err := os.WriteFile(markerPath,
		[]byte("zon-cacher-v"+cacheFormatVersion+"-legacy\n"), 0644); err != nil {
		return err
	}
	return processDirectory(dirPath, cfg)
}

// ============================================================================
// paths 过滤 (关键修复)
// ============================================================================

func readPackagePaths(pkgDir string) []string {
	zonPath := filepath.Join(pkgDir, "build.zig.zon")
	content, err := os.ReadFile(zonPath)
	if err != nil {
		return nil
	}
	return extractStringList(stripZONComments(string(content)), ".paths")
}

// filterTree 按 zig 的 .paths 白名单语义删除根之外的文件/目录。
// paths 为空或包含 "" (即 .{""}) 表示不过滤。
// build.zig 与 build.zig.zon 始终保留（zig 隐式包含）。
func filterTree(root string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	keep := make(map[string]bool)
	for _, p := range paths {
		p = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(p)), "/")
		if p == "" || p == "." {
			return nil // .{""} = 全部保留
		}
		if strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
			continue
		}
		keep[p] = true
	}
	if len(keep) == 0 {
		return nil
	}
	keep["build.zig"] = true
	keep["build.zig.zon"] = true

	var walk func(dir string) (bool, error)
	walk = func(dir string) (bool, error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return false, err
		}
		anyKept := false
		for _, e := range entries {
			full := filepath.Join(dir, e.Name())
			rel, err := filepath.Rel(root, full)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if e.IsDir() {
				subKept, err := walk(full)
				if err != nil {
					return false, err
				}
				if subKept || keep[rel] || isUnderAnyKeep(rel, keep) {
					anyKept = true
				} else {
					if err := os.RemoveAll(full); err != nil {
						return false, err
					}
				}
			} else {
				if keep[rel] || isUnderAnyKeep(rel, keep) {
					anyKept = true
				} else {
					if err := os.Remove(full); err != nil {
						return false, err
					}
				}
			}
		}
		return anyKept, nil
	}
	_, err := walk(root)
	return err
}

func isUnderAnyKeep(rel string, keep map[string]bool) bool {
	for p := range keep {
		if strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// ============================================================================
// 规范 tar.gz 打包 (关键修复): 确定性输出，与 zig 重新压缩的规范 blob 对齐
// ============================================================================

func writeCanonicalTarGz(root, destPath string) error {
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	bw := bufio.NewWriter(f)
	gw, err := gzip.NewWriterLevel(bw, gzip.BestCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gw)

	// WalkDir 按字典序遍历，保证条目顺序确定
	type entry struct{ path, rel string }
	var entries []entry
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		entries = append(entries, entry{path: p, rel: filepath.ToSlash(rel)})
		return nil
	})
	if err != nil {
		return err
	}

	zero := time.Unix(0, 0)
	for _, e := range entries {
		info, err := os.Lstat(e.path)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			ModTime: zero,
			Uid:     0, Gid: 0,
			Uname: "", Gname: "",
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(e.path)
			if err != nil {
				return err
			}
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = link
			hdr.Name = e.rel
			hdr.Mode = 0o777
		case info.IsDir():
			hdr.Typeflag = tar.TypeDir
			hdr.Name = e.rel + "/"
			hdr.Mode = 0o755
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Name = e.rel
			hdr.Size = info.Size() // <-- 关键修复: 必须声明文件大小，否则 tar writer 报 "write too long"
			if info.Mode()&0o111 != 0 {
				hdr.Mode = 0o755
			} else {
				hdr.Mode = 0o644
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			src, err := os.Open(e.path)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, src)
			src.Close()
			if err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}
	return bw.Flush()
}

// ============================================================================
// 下载与解压
// ============================================================================

func downloadToFile(url, destPath string) error {
	// 修复: 剥离 URL 片段，避免 tarball URL 带 #fragment 时请求失败
	if i := strings.IndexByte(url, '#'); i >= 0 {
		url = url[:i]
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "zon-cacher/"+cacheFormatVersion)

	var resp *http.Response
	for attempt := 1; attempt <= 3; attempt++ {
		resp, err = httpClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		if attempt < 3 {
			time.Sleep(2 * time.Second)
		}
	}
	if err != nil {
		return fmt.Errorf("HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func downloadAndExtractArchive(url, destDir string) error {
	tmpFile, err := os.CreateTemp("", "zon-archive-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	if err := downloadToFile(url, tmpFile.Name()); err != nil {
		return err
	}

	lower := strings.ToLower(url)
	var cmd *exec.Cmd
	switch {
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		cmd = exec.Command("tar", "-xzf", tmpFile.Name(), "-C", destDir)
	case strings.HasSuffix(lower, ".tar.xz"):
		cmd = exec.Command("tar", "-xJf", tmpFile.Name(), "-C", destDir)
	case strings.HasSuffix(lower, ".tar.zst"):
		return extractTarZstd(tmpFile.Name(), destDir)
	case strings.HasSuffix(lower, ".zip"):
		cmd = exec.Command("unzip", "-q", tmpFile.Name(), "-d", destDir)
	default:
		cmd = exec.Command("tar", "-xf", tmpFile.Name(), "-C", destDir)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func extractTarZstd(zstPath, destDir string) error {
	cmd := exec.Command("tar", "--zstd", "-xf", zstPath, "-C", destDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err == nil {
		return nil
	}

	tmpTar, err := os.CreateTemp("", "zon-zstd-*.tar")
	if err != nil {
		return err
	}
	tmpTarPath := tmpTar.Name()
	tmpTar.Close()
	defer os.Remove(tmpTarPath)

	zstdCmd := exec.Command("zstd", "-d", "-q", "-o", tmpTarPath, zstPath)
	if err := zstdCmd.Run(); err != nil {
		return fmt.Errorf("zstd 解压失败: %w", err)
	}
	tarCmd := exec.Command("tar", "-xf", tmpTarPath, "-C", destDir)
	return tarCmd.Run()
}

// ============================================================================
// Git
// ============================================================================

func downloadGit(url, dest string) error {
	cleanURL := strings.TrimPrefix(url, "git+")
	parts := strings.SplitN(cleanURL, "#", 2)
	repoURL := parts[0]

	env := os.Environ()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		if val := os.Getenv(key); val != "" {
			env = append(env, strings.ToLower(key)+"="+val)
		}
	}
	if proxy := os.Getenv("HTTPS_PROXY"); proxy != "" {
		env = append(env, "GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=https.proxy",
			"GIT_CONFIG_VALUE_0="+proxy)
	}

	if len(parts) == 1 {
		cmd := exec.Command("git", "clone", "--depth", "1", repoURL, dest)
		cmd.Env = env
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	commitHash := parts[1]

	initCmd := exec.Command("git", "init", dest)
	initCmd.Env = env
	if err := initCmd.Run(); err != nil {
		return err
	}
	remoteCmd := exec.Command("git", "remote", "add", "origin", repoURL)
	remoteCmd.Dir = dest
	remoteCmd.Env = env
	if err := remoteCmd.Run(); err != nil {
		return err
	}

	fetchCmd := exec.Command("git", "fetch", "--depth", "1", "origin", commitHash)
	fetchCmd.Dir = dest
	fetchCmd.Env = env
	fetchCmd.Stdout = os.Stdout
	fetchCmd.Stderr = os.Stderr
	if err := fetchCmd.Run(); err != nil {
		fb := exec.Command("git", "fetch", "origin")
		fb.Dir = dest
		fb.Env = env
		fb.Stdout = os.Stdout
		fb.Stderr = os.Stderr
		if err := fb.Run(); err != nil {
			return err
		}
	}

	checkoutCmd := exec.Command("git", "checkout", commitHash)
	checkoutCmd.Dir = dest
	checkoutCmd.Env = env
	checkoutCmd.Stdout = os.Stdout
	checkoutCmd.Stderr = os.Stderr
	if err := checkoutCmd.Run(); err != nil {
		return err
	}

	os.RemoveAll(filepath.Join(dest, ".git"))
	return nil
}

// ============================================================================
// 文件移动/复制（跨设备安全）
// ============================================================================

func movePath(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// 跨设备: 复制后删除
	if err := copyTree(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

// ============================================================================
// 辅助
// ============================================================================

func determineSourceDir(tmpDir string) string {
	entries, err := os.ReadDir(tmpDir)
	if err == nil && len(entries) == 1 && entries[0].IsDir() {
		return filepath.Join(tmpDir, entries[0].Name())
	}
	return tmpDir
}

func isZstdURL(url string) bool {
	return strings.HasSuffix(strings.ToLower(url), ".tar.zst")
}

func isArchiveURL(url string) bool {
	lower := strings.ToLower(url)
	return strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") ||
		strings.HasSuffix(lower, ".tar.xz") || strings.HasSuffix(lower, ".tar.zst") ||
		strings.HasSuffix(lower, ".zip") || strings.Contains(lower, "/archive/") ||
		strings.Contains(lower, "/releases/download/")
}

func isGitURL(url string) bool {
	if isArchiveURL(url) {
		return false
	}
	lower := strings.ToLower(url)
	return strings.HasPrefix(url, "git+") || strings.HasPrefix(url, "git://") ||
		strings.HasSuffix(lower, ".git") || strings.Contains(url, "#")
}

// ============================================================================
// ZON 解析 (增强: 支持解析 .paths 列表、字段键精确定位)
// ============================================================================

func parseZON(content string) []Dependency {
	cleaned := stripZONComments(content)
	var deps []Dependency
	idx := strings.Index(cleaned, ".dependencies")
	if idx == -1 {
		return deps
	}
	braceStart := strings.Index(cleaned[idx:], "{")
	if braceStart == -1 {
		return deps
	}
	braceStart += idx
	block, _ := extractBlock(cleaned, braceStart)
	if block == "" {
		return deps
	}

	i := 0
	for i < len(block) {
		for i < len(block) && (block[i] == ' ' || block[i] == '\t' || block[i] == '\n' || block[i] == '\r' || block[i] == ',') {
			i++
		}
		if i >= len(block) || block[i] != '.' {
			i++
			continue
		}
		i++
		var name string
		if i < len(block) && block[i] == '"' {
			i++
			start := i
			for i < len(block) && block[i] != '"' {
				if block[i] == '\\' {
					i++
				}
				i++
			}
			name = block[start:i]
			if i < len(block) {
				i++
			}
		} else {
			start := i
			for i < len(block) && (isAlnum(block[i]) || block[i] == '_') {
				i++
			}
			name = block[start:i]
		}
		for i < len(block) && isSpace(block[i]) {
			i++
		}
		if i >= len(block) || block[i] != '=' {
			continue
		}
		i++
		for i < len(block) && isSpace(block[i]) {
			i++
		}
		if i >= len(block) || block[i] != '.' {
			continue
		}
		i++
		for i < len(block) && isSpace(block[i]) {
			i++
		}
		if i >= len(block) || block[i] != '{' {
			continue
		}
		itemBlock, next := extractBlock(block, i)
		if itemBlock == "" {
			break
		}
		i = next
		dep := Dependency{Name: name}
		dep.URL = extractStringValue(itemBlock, ".url")
		dep.Hash = extractStringValue(itemBlock, ".hash")
		dep.Path = extractStringValue(itemBlock, ".path")
		dep.Paths = extractStringList(itemBlock, ".paths")
		if dep.URL != "" && dep.Hash != "" {
			deps = append(deps, dep)
		} else if dep.Path != "" {
			deps = append(deps, dep)
		}
	}
	return deps
}

func stripZONComments(content string) string {
	var sb strings.Builder
	sb.Grow(len(content))
	inString, escaped := false, false
	for i := 0; i < len(content); i++ {
		b := content[i]
		if inString {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == '"' {
				inString = false
			}
			sb.WriteByte(b)
			continue
		}
		if b == '"' {
			inString = true
			sb.WriteByte(b)
			continue
		}
		if b == '/' && i+1 < len(content) && content[i+1] == '/' {
			for i < len(content) && content[i] != '\n' {
				i++
			}
			if i < len(content) {
				sb.WriteByte('\n')
			}
			continue
		}
		sb.WriteByte(b)
	}
	return sb.String()
}

func extractBlock(s string, start int) (string, int) {
	if start >= len(s) || s[start] != '{' {
		return "", start
	}
	depth := 0
	i := start
	for i < len(s) {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start+1 : i], i + 1
			}
		case '"':
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' {
					i++
				}
				i++
			}
		}
		i++
	}
	return "", len(s)
}

// findKeyPos 精确定位形如 ".url" 的字段键（避免误匹配字符串字面量里的子串）。
// 要求键前面是空白/逗号/'{'，且键后面（可带空白）紧跟 '='。
func findKeyPos(s, key string) int {
	from := 0
	for {
		rel := strings.Index(s[from:], key)
		if rel < 0 {
			return -1
		}
		idx := from + rel
		prevOK := idx == 0 || isSpace(s[idx-1]) || s[idx-1] == ',' || s[idx-1] == '{'
		if prevOK {
			j := idx + len(key)
			for j < len(s) && isSpace(s[j]) {
				j++
			}
			if j < len(s) && s[j] == '=' {
				return idx
			}
		}
		from = idx + 1
	}
}

func extractStringValue(s, key string) string {
	idx := findKeyPos(s, key)
	if idx == -1 {
		return ""
	}
	i := idx + len(key)
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	if i >= len(s) || s[i] != '=' {
		return ""
	}
	i++
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	if i >= len(s) || s[i] != '"' {
		return ""
	}
	i++
	start := i
	for i < len(s) && s[i] != '"' {
		if s[i] == '\\' {
			i++
		}
		i++
	}
	return s[start:i]
}

// extractStringList 解析 `.paths = .{ "a", "b" }` 形式的字符串列表
func extractStringList(s, key string) []string {
	idx := findKeyPos(s, key)
	if idx == -1 {
		return nil
	}
	i := idx + len(key)
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	if i >= len(s) || s[i] != '=' {
		return nil
	}
	i++
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	if i >= len(s) || s[i] != '.' {
		return nil
	}
	i++
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	if i >= len(s) || s[i] != '{' {
		return nil
	}
	block, _ := extractBlock(s, i)
	var out []string
	j := 0
	for j < len(block) {
		if block[j] == '"' {
			j++
			start := j
			for j < len(block) && block[j] != '"' {
				if block[j] == '\\' {
					j++
				}
				j++
			}
			out = append(out, block[start:j])
			if j < len(block) {
				j++
			}
		} else {
			j++
		}
	}
	return out
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func getGlobalCacheDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("LocalAppData"), "zig")
	}
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, "zig")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "zig")
}
