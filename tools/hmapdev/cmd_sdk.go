package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const sdkDirName = "hmapdev/sdk"

// legacySDKDirName 是改名前的存储目录。工具链在 1.2.0 从 plugindev 改名 hmapdev；
// 已装过旧版的机器上 SDK 仍在旧路径，直接换名会让它找不到已装 SDK
// （表现为「没有活动版本」）。新目录不存在而旧目录存在时沿用旧目录。
const legacySDKDirName = "plugindev/sdk"

// storeDirName 是 SDK store 的根目录名（~/.hologram）。
// legacyStoreDirName 是产品改名前的根目录名（~/.homeagent）。
//
// 用户机器上可能两种情况都有：新版装到新根目录，而**改名之前**装的 SDK 还在
// 旧根目录里。所以新目录不存在、旧目录存在时沿用旧的——否则用户升级工具链后
// 会看到「没有活动版本」，而磁盘上那份 SDK 明明还在。
const (
	storeDirName       = ".hologram"
	legacyStoreDirName = ".homeagent"
)

// sdkStore returns the root directory for stored SDK versions.
func sdkStore() string {
	// 环境变量优先。新名为主，旧名继续认（存量脚本/文档还在用旧名）。
	if v := os.Getenv("HOLOGRAM_SDK_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("HOMEAGENT_SDK_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("error: cannot determine home directory: %v\n", err)
		os.Exit(1)
	}
	dir := filepath.Join(home, storeDirName, sdkDirName)
	if _, err := os.Stat(dir); err != nil {
		// 1) 同一根目录下更早的工具链时代目录（plugindev/sdk）
		for _, legacy := range []string{
			filepath.Join(home, storeDirName, legacySDKDirName),
			// 2) 产品改名前的整根目录（~/.homeagent/...）
			filepath.Join(home, legacyStoreDirName, sdkDirName),
			filepath.Join(home, legacyStoreDirName, legacySDKDirName),
		} {
			if _, err := os.Stat(legacy); err == nil {
				return legacy
			}
		}
	}
	return dir
}

func sdkCurrentDir() string {
	return filepath.Join(sdkStore(), "current")
}

func sdkVersionDir(version string) string {
	return filepath.Join(sdkStore(), version)
}

const sdkRepoURL = "https://github.com/JianFeeeee/hologramagentsdk.git"

// sdkDownloadURL 是发布归档地址（%s = tag）。
// 归档里的顶层目录名带仓库名，必须跟着仓库改名一起改，否则解出来的目录名对不上。
const sdkDownloadURL = "https://github.com/JianFeeeee/hologramagentsdk/-/archive/%s/hologramagent-sdk-%s.tar.gz"

func cmdSDK(args []string) {
	if len(args) < 1 {
		sdkHelp()
		return
	}
	// install --from <本地目录> [version]：用本地 SDK 源码装一个版本并激活。
	if args[0] == "install" {
		from := ""
		rest := []string{}
		for i := 1; i < len(args); i++ {
			if args[i] == "--from" && i+1 < len(args) {
				from = args[i+1]
				i++
				continue
			}
			rest = append(rest, args[i])
		}
		if from != "" {
			version := ""
			if len(rest) > 0 && rest[0] != "latest" {
				version = rest[0]
			}
			cmdSDKInstallFromDir(from, version)
			return
		}
	}
	switch args[0] {
	case "list":
		cmdSDKList()
	case "install":
		if len(args) < 2 {
			fmt.Println("Usage: hmapdev sdk install <version>")
			os.Exit(1)
		}
		cmdSDKInstall(args[1])
	case "use":
		if len(args) < 2 {
			fmt.Println("Usage: hmapdev sdk use <version>")
			os.Exit(1)
		}
		cmdSDKUse(args[1])
	case "path":
		cmdSDKPath()
	case "current":
		cmdSDKCurrent()
	case "latest":
		cmdSDKLatest()
	default:
		sdkHelp()
	}
}

func sdkHelp() {
	fmt.Print(`Usage: hmapdev sdk <command>

Manage installed HologramAgent SDK versions.

Commands:
  list                  List installed SDK versions
  install <version>     Download and install an SDK version (tag or branch)
  use <version>         Switch to the specified SDK version for new projects
  path                  Show the current active SDK directory
  current               Show the current active SDK version
  latest                Show the latest available version from remote

Examples:
  hmapdev sdk install v0.7.1
  hmapdev sdk install latest
  hmapdev sdk install v0.7.1
  hmapdev sdk use v0.7.1
  hmapdev sdk install --from /path/to/hologramagent-sdk   # 用本地源码（SDK 开发时用）
`)
}

// cmdSDKList lists installed SDK versions.
func cmdSDKList() {
	store := sdkStore()
	entries, err := os.ReadDir(store)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No SDK versions installed.")
			return
		}
		fmt.Printf("error: read SDK store %s: %v\n", store, err)
		os.Exit(1)
	}

	current := resolveCurrentVersion(store)

	var versions []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != "current" && !strings.HasPrefix(e.Name(), ".") {
			versions = append(versions, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(versions)))

	if len(versions) == 0 {
		fmt.Println("No SDK versions installed.")
		return
	}
	fmt.Println("Installed SDK versions:")
	for _, v := range versions {
		mark := " "
		if v == current {
			mark = "*"
		}
		fmt.Printf("  %s %s\n", mark, v)
	}
	if current == "" {
		fmt.Println("\nNo version active. Use 'hmapdev sdk use <version>' to set one.")
	}
}

// cmdSDKInstallFromDir 从**本地 SDK 源码目录**安装一个版本。
//
// 为什么需要它：`install` 只能从 Release 归档下载，而 SDK 开发时的新能力
// （例如 `InjectOptions.Priority` 这类 proc 桥要透传的字段）往往还没发版 ——
// 此时生成出来的插件工程会因为"引用的 SDK 还没有该字段"直接编译失败。
// 有 --from 才能"用本地源码当这个版本的 SDK"，边改 SDK 边验证模板工程。
func cmdSDKInstallFromDir(src, version string) {
	store := sdkStore()
	if err := os.MkdirAll(store, 0755); err != nil {
		fmt.Printf("error: create SDK store %s: %v\n", store, err)
		os.Exit(1)
	}
	if version == "" {
		version = readMetaVersion(src)
	}
	if version == "" {
		fmt.Printf("error: cannot determine version from %s/meta/meta.go\n", src)
		os.Exit(1)
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	if _, err := os.Stat(filepath.Join(src, "go.mod")); err != nil {
		fmt.Printf("error: %s 看起来不是 SDK 源码目录（缺 go.mod）\n", src)
		os.Exit(1)
	}
	dest := sdkVersionDir(version)
	_ = os.RemoveAll(dest)
	if err := copyDir(src, dest); err != nil {
		fmt.Printf("error: copy %s -> %s: %v\n", src, dest, err)
		os.Exit(1)
	}
	// 源码目录里的开发产物不该带进 store。
	for _, junk := range []string{".git", "dist", "build"} {
		_ = os.RemoveAll(filepath.Join(dest, junk))
	}
	fmt.Printf("Installed SDK %s from %s\n", version, src)
	fmt.Printf("  %s\n", dest)
	if err := os.WriteFile(filepath.Join(store, "current"), []byte(version), 0644); err != nil {
		fmt.Printf("error: activate %s: %v\n", version, err)
		os.Exit(1)
	}
	fmt.Printf("Activated SDK %s\n", version)
}

// cmdSDKInstall downloads and installs an SDK version from Release archive.
func cmdSDKInstall(version string) {
	store := sdkStore()
	if err := os.MkdirAll(store, 0755); err != nil {
		fmt.Printf("error: create SDK store %s: %v\n", store, err)
		os.Exit(1)
	}

	if version == "latest" {
		tag, err := fetchLatestTag()
		if err != nil {
			fmt.Printf("error: fetch latest tag: %v\n", err)
			os.Exit(1)
		}
		version = tag
		fmt.Printf("Latest version: %s\n", version)
	}

	dest := sdkVersionDir(version)
	if _, err := os.Stat(dest); err == nil {
		fmt.Printf("SDK version %s already installed at %s\n", version, dest)
		return
	}

	url := fmt.Sprintf(sdkDownloadURL, version, version)
	fmt.Printf("Downloading SDK %s from Release archive...\n", version)

	tmpDir, err := os.MkdirTemp("", "hologramagent-sdk-extract-*")
	if err != nil {
		fmt.Printf("error: create temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	if err := installFromArchive(url, tmpDir); err != nil {
		fmt.Printf("warn: archive download failed (%v), falling back to git clone...\n", err)
		if err := installFromGit(version, tmpDir); err != nil {
			fmt.Printf("error: install SDK %s: %v\n", version, err)
			os.Exit(1)
		}
	}

	if err := os.Rename(tmpDir, dest); err != nil {
		// Cross-filesystem rename fallback
		if err := copyDir(tmpDir, dest); err != nil {
			fmt.Printf("error: move SDK to store: %v\n", err)
			os.Exit(1)
		}
		os.RemoveAll(tmpDir)
	}

	fmt.Printf("SDK version %s installed at %s\n", version, dest)

	// Auto-switch to newly installed version if no version is currently active
	if resolveCurrentVersion(store) == "" {
		setCurrentVersion(store, version)
	}
}

func openFile(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// installFromArchive downloads the SDK release archive and extracts it to tmpDir.
func installFromArchive(url, tmpDir string) error {
	tmpFile, err := os.CreateTemp("", "hologramagent-sdk-*.tar.gz")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download SDK %s: HTTP %d", url, resp.StatusCode)
	}

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		return err
	}
	tmpFile.Close()

	f, err := openFile(tmpPath)
	if err != nil {
		return err
	}
	gzr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return err
	}
	defer gzr.Close()
	defer f.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Strip top-level directory from archive path
		parts := strings.SplitN(header.Name, "/", 2)
		if len(parts) < 2 {
			continue
		}
		relPath := parts[1]
		if relPath == "" {
			continue
		}
		target := filepath.Join(tmpDir, relPath)

		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, os.FileMode(header.Mode))
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0755)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}

// installFromGit clones the SDK repo at the given tag/branch into tmpDir.
func installFromGit(version, tmpDir string) error {
	cmd := exec.Command("git", "clone", "--depth", "1", "--branch", version, sdkRepoURL, tmpDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone: %v", err)
	}
	return os.RemoveAll(filepath.Join(tmpDir, ".git"))
}

// cmdSDKUse switches the active SDK version.
func cmdSDKUse(version string) {
	store := sdkStore()
	verDir := sdkVersionDir(version)
	if _, err := os.Stat(verDir); os.IsNotExist(err) {
		fmt.Printf("SDK version %s is not installed.\n", version)
		fmt.Printf("Install it first: hmapdev sdk install %s\n", version)
		os.Exit(1)
	}
	setCurrentVersion(store, version)
	fmt.Printf("Active SDK version set to %s\n", version)
}

// cmdSDKPath prints the active SDK directory.
func cmdSDKPath() {
	store := sdkStore()
	current := resolveCurrentVersion(store)
	if current == "" {
		fmt.Println("No active SDK version set.")
		fmt.Println("Use 'hmapdev sdk use <version>' to set one.")
		os.Exit(1)
	}
	fmt.Println(sdkVersionDir(current))
}

// cmdSDKCurrent prints the active SDK version.
func cmdSDKCurrent() {
	store := sdkStore()
	current := resolveCurrentVersion(store)
	if current == "" {
		fmt.Println("No active SDK version set.")
		os.Exit(1)
	}
	fmt.Println(current)
}

// cmdSDKLatest fetches the latest tag from the remote SDK repo.
func cmdSDKLatest() {
	tag, err := fetchLatestTag()
	if err != nil {
		fmt.Printf("error: fetch latest tag: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(tag)
}

// resolveCurrentVersion reads the current active version.
func resolveCurrentVersion(store string) string {
	currentFile := filepath.Join(store, "current")
	data, err := os.ReadFile(currentFile)
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return ""
	}
	verDir := filepath.Join(store, v)
	if _, err := os.Stat(verDir); os.IsNotExist(err) {
		return ""
	}
	return v
}

// setCurrentVersion writes the version name to the current file.
func setCurrentVersion(store, version string) {
	currentFile := filepath.Join(store, "current")
	if err := os.WriteFile(currentFile, []byte(version+"\n"), 0644); err != nil {
		fmt.Printf("error: write current version: %v\n", err)
		os.Exit(1)
	}
}

// fetchLatestTag uses git ls-remote to find the latest semver tag.
func fetchLatestTag() (string, error) {
	cmd := exec.Command("git", "ls-remote", "--tags", sdkRepoURL)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git ls-remote failed: %w", err)
	}

	tags := parseTags(string(out))
	if len(tags) == 0 {
		return "", fmt.Errorf("no tags found in remote repository")
	}
	return tags[len(tags)-1], nil
}

// parseTags extracts semver tags from git ls-remote output and sorts them.
func parseTags(output string) []string {
	seen := make(map[string]bool)
	var tags []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		ref := parts[1]
		// Only refs/tags/v*, skip refs/tags/v*-^{}
		if !strings.HasPrefix(ref, "refs/tags/v") || strings.HasSuffix(ref, "^{}") {
			continue
		}
		tag := strings.TrimPrefix(ref, "refs/tags/")
		if seen[tag] {
			continue
		}
		seen[tag] = true
		tags = append(tags, tag)
	}

	sort.Slice(tags, func(i, j int) bool {
		return compareSemver(tags[i], tags[j]) < 0
	})
	return tags
}

// compareSemver compares two semver tags (vX.Y.Z). Returns -1, 0, or 1.
func compareSemver(a, b string) int {
	an := parseSemver(a)
	bn := parseSemver(b)
	for i := 0; i < 3; i++ {
		if an[i] != bn[i] {
			if an[i] < bn[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// parseSemver extracts [major, minor, patch] from a vX.Y.Z[-pre] string.
// Prerelease tags parse to the same major.minor.patch as their release (ignoring prerelease).
func parseSemver(tag string) [3]int {
	var v [3]int
	s := strings.TrimPrefix(tag, "v")
	// Strip prerelease suffix (-...)
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		s = s[:idx]
	}
	parts := strings.SplitN(s, ".", 3)
	for i := 0; i < 3 && i < len(parts); i++ {
		n := 0
		fmt.Sscanf(parts[i], "%d", &n)
		v[i] = n
	}
	return v
}

// activeSDKRoot returns the path to the active SDK root.
// It replaces the old runtime.Caller(0) approach so hmapdev can work
// independently of its own build location.
func activeSDKRoot() string {
	store := sdkStore()
	current := resolveCurrentVersion(store)
	if current == "" {
		fmt.Printf("error: no active SDK version set\n")
		fmt.Printf("  Install one: hmapdev sdk install latest\n")
		fmt.Printf("  Or set one:  hmapdev sdk use <version>\n")
		os.Exit(1)
	}
	root := sdkVersionDir(current)
	if _, err := os.Stat(root); os.IsNotExist(err) {
		fmt.Printf("error: active SDK version %s not found at %s\n", current, root)
		fmt.Printf("  Reinstall: hmapdev sdk install %s\n", current)
		os.Exit(1)
	}
	return root
}

// copyDir recursively copies src to dst (cross-filesystem rename fallback).
func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			data, err := os.ReadFile(srcPath)
			if err != nil {
				return err
			}
			if err := os.WriteFile(dstPath, data, 0644); err != nil {
				return err
			}
		}
	}
	return nil
}

// readMetaVersion reads the Version string from the SDK's meta/meta.go.
// If the file is missing or unreadable, returns "0.0.0".
func readMetaVersion(sdkRoot string) string {
	metaPath := filepath.Join(sdkRoot, "meta", "meta.go")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return "0.0.0"
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Version = ") {
			v := strings.TrimPrefix(line, "Version = ")
			v = strings.Trim(v, `"`)
			return v
		}
	}
	return "0.0.0"
}
