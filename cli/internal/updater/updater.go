/*
 * Copyright (c) Huawei Technologies Co., Ltd. 2026-2027. All rights reserved.
 */

package updater

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"time"
)

const (
	gitCodeAPI       = "https://gitcode.com/api/v5/repos/CloudDeveloperDepartment/devbrige/releases"
	gitCodeInstallSh = "https://gitcode.com/CloudDeveloperDepartment/devbrige/releases/download/latest/install.sh"
	gitCodeInstallPs = "https://gitcode.com/CloudDeveloperDepartment/devbrige/releases/download/latest/install.ps1"
	cacheTTL         = 24 * time.Hour
	httpTimeout      = 10 * time.Second
)

var (
	// 宽松匹配版本号：支持 v0.1.9 / 0.1.9 / 0.1.13.333 / 0.1.3-release / 0.1.3.release 等。
	// 只解析开头的数字段（major.minor.patch[.build]），忽略任意后缀。
	versionRegex = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:\.(\d+))?`)
)

// CheckResult holds the version check result.
type CheckResult struct {
	LatestVersion string `json:"latestVersion"`
	LatestTag     string `json:"latestTag"`
	CheckedAt     int64  `json:"checkedAt"`
	LastNotifyAt  int64  `json:"lastNotifyAt"` // last time async notice was printed
}

// gitcodeRelease represents a single release from the GitCode API.
type gitcodeRelease struct {
	TagName string `json:"tag_name"`
}

// IsNewer compares current version with latest. Returns true if latest > current.
// 版本号可以是 "0.1.9"、"0.1.13.333"（四段 build 号）、"0.1.3-release"、"0.1.3.release"，
// 只比较开头的主版本/次版本/修订号/构建号，后缀被忽略。
func IsNewer(current, latest string) bool {
	return isNewerRaw(current, latest)
}

func parseVersion(v string) (major, minor, patch, build int, ok bool) {
	m := versionRegex.FindStringSubmatch(v)
	if m == nil {
		return 0, 0, 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	patch, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		build, _ = strconv.Atoi(m[4])
	}
	return major, minor, patch, build, true
}

// versionFromTag 从 tag 中提取规范化的数字版本号（如 "0.2.0-release" → "0.2.0"，
// "0.1.13.333" → "0.1.13.333"），忽略后缀。非版本 tag（如 "latest"、"test-*"）返回 !ok。
func versionFromTag(s string) (string, bool) {
	m := versionRegex.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	v := m[1] + "." + m[2] + "." + m[3]
	if m[4] != "" {
		v += "." + m[4]
	}
	return v, true
}

// cachePath returns the path to the version cache file.
func cachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".huawei", "devbridge", "version_cache.json"), nil
}

// loadCache reads the cached check result if still valid.
func loadCache() *CheckResult {
	path, err := cachePath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var r CheckResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil
	}
	if time.Since(time.Unix(r.CheckedAt, 0)) > cacheTTL {
		return nil
	}
	return &r
}

// saveCache writes the check result to the cache file.
func saveCache(r *CheckResult) {
	path, err := cachePath()
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	data, _ := json.Marshal(r)
	_ = os.WriteFile(path, data, 0o600)
}

// fetchLatestRelease queries the GitCode API and returns the latest release tag.
func fetchLatestRelease() (string, error) {
	client := &http.Client{Timeout: httpTimeout}
	req, err := http.NewRequest("GET", gitCodeAPI, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gitcode API returned %d", resp.StatusCode)
	}
	var releases []gitcodeRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return "", err
	}
	var bestTag string
	var bestVer string
	for _, r := range releases {
		if !versionRegex.MatchString(r.TagName) {
			continue
		}
		ver, ok := versionFromTag(r.TagName)
		if !ok {
			continue
		}
		if bestVer == "" || isNewerRaw(bestVer, ver) {
			bestVer = ver
			bestTag = r.TagName
		}
	}
	if bestTag == "" {
		return "", fmt.Errorf("no valid release found")
	}
	return bestTag, nil
}

func isNewerRaw(oldV, newV string) bool {
	omaj, omin, opat, obuild, ok1 := parseVersion(oldV)
	nmaj, nmin, npat, nbuild, ok2 := parseVersion(newV)
	if !ok1 || !ok2 {
		return false
	}
	if nmaj != omaj {
		return nmaj > omaj
	}
	if nmin != omin {
		return nmin > omin
	}
	if npat != opat {
		return npat > opat
	}
	return nbuild > obuild
}

// Check performs a version check, using cache when available.
func Check(currentVersion string) *CheckResult {
	// Try cache first
	if cached := loadCache(); cached != nil {
		return cached
	}

	// Fetch latest release
	latestTag, err := fetchLatestRelease()
	if err != nil {
		return nil
	}

	// Extract version number from tag
	latestVersion := latestTag
	if v, ok := versionFromTag(latestTag); ok {
		latestVersion = v
	}

	result := &CheckResult{
		LatestVersion: latestVersion,
		LatestTag:     latestTag,
		CheckedAt:     time.Now().Unix(),
	}
	saveCache(result)
	return result
}

// CheckAsync runs the version check in a goroutine and prints a notice if a new version is available.
// It should be called from PersistentPreRun — it does not block command execution.
// The async notice is printed at most once per day.
func CheckAsync(currentVersion string) {
	asyncWG.Add(1)
	go func() {
		defer asyncWG.Done()
		result := Check(currentVersion)
		if result == nil {
			return
		}
		if !IsNewer(currentVersion, result.LatestVersion) {
			return
		}
		// Rate-limit async notice to once per day
		if result.LastNotifyAt > 0 && time.Since(time.Unix(result.LastNotifyAt, 0)) < 24*time.Hour {
			return
		}
		result.LastNotifyAt = time.Now().Unix()
		saveCache(result)
		fmt.Fprintf(os.Stderr, "\nA new version is available: %s (current: %s)\nUpdate:\n%s\n\n",
			result.LatestVersion, currentVersion, InstallCommand())
	}()
}

// asyncWG tracks in-flight async version checks so the process can wait briefly
// for them before exiting (otherwise fast commands like `list` terminate before
// the goroutine finishes its network fetch).
var asyncWG sync.WaitGroup

// WaitAsync blocks up to timeout for any pending async version check to finish.
// It should be called from main() after Execute() returns, so a quick command
// still gets a chance to print the update notice without holding up long commands.
func WaitAsync(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		asyncWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// CheckSync runs the version check synchronously and returns the result.
// Used by the `version` command.
func CheckSync(currentVersion string) *CheckResult {
	return Check(currentVersion)
}

// ForceCheck bypasses the cache and performs a fresh check.
func ForceCheck(currentVersion string) *CheckResult {
	// Delete cache
	path, err := cachePath()
	if err == nil {
		_ = os.Remove(path)
	}
	return Check(currentVersion)
}

// InstallCommand returns the one-liner install commands for the current platform.
func InstallCommand() string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("  # GitCode\n  irm %s | iex\n\n  # GitHub\n  irm https://github.com/huaweicloud/devspace-devbridge/releases/latest/download/install.ps1 | iex",
			gitCodeInstallPs)
	}
	return fmt.Sprintf("  # GitCode\n  curl -fsSL %s | bash\n\n  # GitHub\n  curl -fsSL https://github.com/huaweicloud/devspace-devbridge/releases/latest/download/install.sh | bash",
		gitCodeInstallSh)
}
