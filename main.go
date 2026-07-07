package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

type Dependency struct {
	Name string
	URL  string
	Hash string
	Path string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("使用方法: zon-cacher <目标目录路径>")
		os.Exit(1)
	}

	targetDir := os.Args[1]
	absPath, err := filepath.Abs(targetDir)
	if err != nil {
		fmt.Printf("错误: 无法获取绝对路径: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("🚀 开始解析目录: %s\n", absPath)
	if err := processDirectory(absPath); err != nil {
		fmt.Printf("❌ 运行过程中出错: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✨ 所有依赖处理完毕！")
}

func processDirectory(dir string) error {
	zonPath := filepath.Join(dir, "build.zig.zon")
	if _, err := os.Stat(zonPath); os.IsNotExist(err) {
		return nil
	}

	fmt.Printf("📦 正在解析: %s\n", zonPath)
	content, err := os.ReadFile(zonPath)
	if err != nil {
		return fmt.Errorf("读取 build.zig.zon 失败: %w", err)
	}

	dependencies := parseZON(string(content))
	if len(dependencies) == 0 {
		return nil
	}

	globalCacheDir := getGlobalCacheDir()

	for _, dep := range dependencies {
		// 处理 .path 本地路径依赖
		if dep.Path != "" {
			localAbsPath := filepath.Clean(filepath.Join(dir, dep.Path))
			fmt.Printf("🔗 发现本地路径依赖 [%s] -> 进入目录: %s\n", dep.Name, localAbsPath)
			if err := processDirectory(localAbsPath); err != nil {
				return err
			}
			continue
		}

		// 远程网络依赖校验
		if dep.Hash == "" {
			fmt.Printf("⚠️ 依赖 [%s] 缺少 hash 值，跳过\n", dep.Name)
			continue
		}

		depCacheDir := filepath.Join(globalCacheDir, "p", dep.Hash)

		if _, err := os.Stat(depCacheDir); !os.IsNotExist(err) {
			fmt.Printf("✅ 依赖已存在 (Cache Hit): %s -> %s\n", dep.Name, dep.Hash)
			if err := processDirectory(depCacheDir); err != nil {
				return err
			}
			continue
		}

		err := func(dep Dependency) error {
			fmt.Printf("🚚 正在下载依赖 [%s]...\n", dep.Name)

			cacheParent := filepath.Dir(depCacheDir)
			if err := os.MkdirAll(cacheParent, 0755); err != nil {
				return err
			}

			tmpDir, err := os.MkdirTemp(cacheParent, "tmp-fetch-*")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmpDir)

			// 根据精准修复后的规则判定类型
			if isGitURL(dep.URL) {
				if err := downloadGit(dep.URL, tmpDir); err != nil {
					return fmt.Errorf("Git 克隆失败 [%s]: %w", dep.Name, err)
				}
			} else {
				if err := downloadAndExtractArchive(dep.URL, tmpDir); err != nil {
					return fmt.Errorf("下载解压失败 [%s]: %w", dep.Name, err)
				}
			}

			sourceDir := determineSourceDir(tmpDir)
			if err := os.Rename(sourceDir, depCacheDir); err != nil {
				return fmt.Errorf("移动到 cache 失败: %w", err)
			}
			fmt.Printf("💾 依赖已成功缓存: %s\n", depCacheDir)

			return processDirectory(depCacheDir)
		}(dep)

		if err != nil {
			return err
		}
	}

	return nil
}

func stripZONComments(content string) string {
	var sb strings.Builder
	sb.Grow(len(content))

	inString := false
	escaped := false

	for i := 0; i < len(content); i++ {
		b := content[i]

		if inString {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == '"' {
				inString = false // 字符串结束
			}
			sb.WriteByte(b)
		} else {
			if b == '"' {
				inString = true // 进入字符串
				sb.WriteByte(b)
			} else if b == '/' && i+1 < len(content) && content[i+1] == '/' {
				// 发现非字符串内部的 //，说明是注释
				// 跳过这一行直到换行符或字符串结束
				for i < len(content) && content[i] != '\n' {
					i++
				}
				// 保留换行符以维持原有行结构
				if i < len(content) && content[i] == '\n' {
					sb.WriteByte('\n')
				}
			} else {
				sb.WriteByte(b)
			}
		}
	}
	return sb.String()
}

func parseZON(content string) []Dependency {
	// 1. 在解析前先清洗掉注释
	cleanedContent := stripZONComments(content)

	var deps []Dependency

	// 2. 后续逻辑使用清洗后的 cleanedContent
	depIdx := strings.Index(cleanedContent, ".dependencies")
	if depIdx == -1 {
		return deps
	}

	startIdx := strings.Index(cleanedContent[depIdx:], "{")
	if startIdx == -1 {
		return deps
	}
	startIdx = depIdx + startIdx

	braceCount := 0
	endIdx := -1
	for i := startIdx; i < len(cleanedContent); i++ {
		if cleanedContent[i] == '{' {
			braceCount++
		} else if cleanedContent[i] == '}' {
			braceCount--
			if braceCount == 0 {
				endIdx = i
				break
			}
		}
	}

	if endIdx == -1 {
		return deps
	}

	dependenciesBlock := cleanedContent[startIdx+1 : endIdx]

	depItemRegex := regexp.MustCompile(`\.(?:([a-zA-Z0-9_]+)|"([^"]+)")\s*=\s*\.\{([^}]+)\}`)
	items := depItemRegex.FindAllStringSubmatch(dependenciesBlock, -1)

	urlRegex := regexp.MustCompile(`\.url\s*=\s*"([^"]+)"`)
	hashRegex := regexp.MustCompile(`\.hash\s*=\s*"([^"]+)"`)
	pathRegex := regexp.MustCompile(`\.path\s*=\s*"([^"]+)"`)

	for _, item := range items {
		name := item[1]
		if name == "" {
			name = item[2]
		}
		itemContent := item[3]

		urlMatch := urlRegex.FindStringSubmatch(itemContent)
		hashMatch := hashRegex.FindStringSubmatch(itemContent)
		pathMatch := pathRegex.FindStringSubmatch(itemContent)

		if len(urlMatch) > 1 && len(hashMatch) > 1 {
			deps = append(deps, Dependency{
				Name: name,
				URL:  urlMatch[1],
				Hash: hashMatch[1],
			})
		} else if len(pathMatch) > 1 {
			deps = append(deps, Dependency{
				Name: name,
				Path: pathMatch[1],
			})
		}
	}
	return deps
}

func getGlobalCacheDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("LocalAppData"), "zig")
	}
	if xdgCache := os.Getenv("XDG_CACHE_HOME"); xdgCache != "" {
		return filepath.Join(xdgCache, "zig")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "zig")
}

// 🔥【核心修复】收紧 Git 判定条件，杜绝把 GitHub 压缩包直链误认为 Git 仓
func isGitURL(url string) bool {
	if strings.HasPrefix(url, "git+") || strings.HasPrefix(url, "git://") || strings.HasSuffix(url, ".git") {
		return true
	}
	// 如果包含 # 锚点，且完全没有压缩包常见特征，则大概率是纯 Git Commit 链接 (如 uucode#5f05f8...)
	if strings.Contains(url, "#") &&
		!strings.Contains(url, ".tar.") &&
		!strings.Contains(url, ".zip") &&
		!strings.Contains(url, "/archive/") {
			return true
		}
		return false
}

func downloadGit(url, dest string) error {
	cleanURL := strings.TrimPrefix(url, "git+")
	parts := strings.SplitN(cleanURL, "#", 2)
	repoURL := parts[0]

	if len(parts) == 1 {
		cmd := exec.Command("git", "clone", "--depth", "1", repoURL, dest)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	commitHash := parts[1]

	initCmd := exec.Command("git", "init", dest)
	if err := initCmd.Run(); err != nil {
		return fmt.Errorf("git init 失败: %w", err)
	}

	remoteCmd := exec.Command("git", "remote", "add", "origin", repoURL)
	remoteCmd.Dir = dest
	if err := remoteCmd.Run(); err != nil {
		return fmt.Errorf("git remote add 失败: %w", err)
	}

	fetchCmd := exec.Command("git", "fetch", "--depth", "1", "origin", commitHash)
	fetchCmd.Dir = dest
	fetchCmd.Stdout = os.Stdout
	fetchCmd.Stderr = os.Stderr
	if err := fetchCmd.Run(); err != nil {
		fallbackFetch := exec.Command("git", "fetch", "origin")
		fallbackFetch.Dir = dest
		fallbackFetch.Stdout = os.Stdout
		fallbackFetch.Stderr = os.Stderr
		if err := fallbackFetch.Run(); err != nil {
			return fmt.Errorf("git fetch 全量回退失败: %w", err)
		}
	}

	checkoutCmd := exec.Command("git", "checkout", commitHash)
	checkoutCmd.Dir = dest
	checkoutCmd.Stdout = os.Stdout
	checkoutCmd.Stderr = os.Stderr
	if err := checkoutCmd.Run(); err != nil {
		return fmt.Errorf("git checkout 失败: %w", err)
	}

	_ = os.RemoveAll(filepath.Join(dest, ".git"))
	return nil
}

func downloadAndExtractArchive(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP 状态码异常: %d, URL: %s", resp.StatusCode, url)
	}

	// 智能推断压缩包后缀
	ext := ".tar.gz"
	if strings.Contains(url, ".zip") {
		ext = ".zip"
	} else if strings.Contains(url, ".tar.xz") {
		ext = ".tar.xz"
	} else if strings.Contains(url, ".tar.zst") {
		ext = ".tar.zst"
	}

	tmpFile, err := os.CreateTemp("", "zig-archive-*"+ext)
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		return err
	}

	var cmd *exec.Cmd
	if ext == ".zip" {
		cmd = exec.Command("unzip", tmpFile.Name(), "-d", dest)
	} else {
		cmd = exec.Command("tar", "-xf", tmpFile.Name(), "-C", dest)
	}

	return cmd.Run()
}

func determineSourceDir(tmpDir string) string {
	files, err := os.ReadDir(tmpDir)
	if err == nil && len(files) == 1 && files[0].IsDir() {
		return filepath.Join(tmpDir, files[0].Name())
	}
	return tmpDir
}
