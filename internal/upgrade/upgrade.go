package upgrade

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	latestURL   = "https://api.github.com/repos/shichao-wang/cpagw/releases/latest"
	downloadURL = "https://github.com/shichao-wang/cpagw/releases/download/"
)

var validTag = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,199}$`)

// Result 区分未更新、已替换和替换后的持久化警告。
type Result struct {
	Version string
	Path    string
	Updated bool
	Warning string
}

type limits struct {
	metadata int64
	checksum int64
	archive  int64
	unpacked int64
	binary   int64
}

type updater struct {
	client     *http.Client
	executable func() (string, error)
	goos       string
	goarch     string
	limits     limits
}

func newUpdater() updater {
	return updater{
		client:     &http.Client{Timeout: 5 * time.Minute, CheckRedirect: checkRedirect},
		executable: os.Executable,
		goos:       runtime.GOOS,
		goarch:     runtime.GOARCH,
		limits: limits{
			metadata: 1 << 20,
			checksum: 1 << 20,
			archive:  128 << 20,
			unpacked: 256 << 20,
			binary:   128 << 20,
		},
	}
}

// Run 从官方最新 Release 下载并校验产物，只替换当前 executable。
func Run(ctx context.Context, currentVersion string) (Result, error) {
	return newUpdater().run(ctx, currentVersion)
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("拒绝非 HTTPS 的下载重定向")
	}
	if len(via) >= 10 {
		return fmt.Errorf("下载重定向次数过多")
	}
	return nil
}

type release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

func archiveName(goos, goarch string) (string, error) {
	if (goos != "darwin" && goos != "linux") || (goarch != "amd64" && goarch != "arm64") {
		return "", fmt.Errorf("不支持的平台：%s/%s，当前仅支持 macOS/Linux 的 amd64/arm64", goos, goarch)
	}
	return fmt.Sprintf("cpagw_%s_%s.tar.gz", goos, goarch), nil
}

func (u updater) run(ctx context.Context, currentVersion string) (Result, error) {
	archive, err := archiveName(u.goos, u.goarch)
	if err != nil {
		return Result{}, err
	}
	data, err := u.fetch(ctx, latestURL, "查询最新 Release", u.limits.metadata)
	if err != nil {
		return Result{}, err
	}
	var latest release
	if err := json.Unmarshal(data, &latest); err != nil || !validTag.MatchString(latest.Tag) || latest.Draft || latest.Prerelease {
		return Result{}, fmt.Errorf("最新 Release 信息无效或不是正式发布")
	}
	result := Result{Version: latest.Tag}
	// 日期-SHA tag 无法可靠排序，以 GitHub latest 为准，只比较是否相同。
	if currentVersion == latest.Tag {
		return result, nil
	}
	for _, name := range []string{archive, "checksums.txt"} {
		count := 0
		for _, asset := range latest.Assets {
			if asset.Name == name {
				count++
			}
		}
		if count != 1 {
			return Result{}, fmt.Errorf("Release 中缺少或重复发布资产：%s", name)
		}
	}

	target, err := locateTarget(u.executable)
	if err != nil {
		return Result{}, err
	}
	workdir, err := os.MkdirTemp(filepath.Dir(target.real), ".cpagw-upgrade-*")
	if err != nil {
		return Result{}, fmt.Errorf("无法在 binary 所在目录创建临时文件，请检查目录写权限或使用安装脚本手动升级")
	}
	defer os.RemoveAll(workdir)

	base := downloadURL + url.PathEscape(latest.Tag) + "/"
	checksums, err := u.fetch(ctx, base+"checksums.txt", "下载校验和", u.limits.checksum)
	if err != nil {
		return Result{}, err
	}
	expected, err := checksumFor(checksums, archive)
	if err != nil {
		return Result{}, err
	}
	compressed, err := os.CreateTemp(workdir, "archive-*")
	if err != nil {
		return Result{}, fmt.Errorf("无法创建安装包临时文件")
	}
	defer compressed.Close()
	hash := sha256.New()
	if err := u.download(ctx, base+archive, io.MultiWriter(compressed, hash), u.limits.archive); err != nil {
		return Result{}, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return Result{}, fmt.Errorf("安装包 SHA256 校验和不匹配，已中止升级")
	}
	if _, err := compressed.Seek(0, io.SeekStart); err != nil {
		return Result{}, fmt.Errorf("无法读取已校验的安装包")
	}

	binary, err := os.CreateTemp(workdir, "binary-*")
	if err != nil {
		return Result{}, fmt.Errorf("无法创建 binary 临时文件")
	}
	defer binary.Close()
	if err := extractBinary(compressed, binary, u.limits); err != nil {
		return Result{}, err
	}
	if err := binary.Chmod(target.info.Mode().Perm()); err != nil {
		return Result{}, fmt.Errorf("无法设置新 binary 的可执行权限")
	}
	if err := binary.Sync(); err != nil {
		return Result{}, fmt.Errorf("无法同步新 binary，已中止升级")
	}
	if err := binary.Close(); err != nil {
		return Result{}, fmt.Errorf("无法关闭新 binary，已中止升级")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("升级已取消：%w", err)
	}
	if err := target.checkUnchanged(); err != nil {
		return Result{}, err
	}
	if err := os.Rename(binary.Name(), target.real); err != nil {
		return Result{}, fmt.Errorf("无法替换当前 binary，请检查安装目录权限；原 binary 未变")
	}

	// Rename 是提交边界，此后失败不能再声称原 binary 未被修改。
	result.Path, result.Updated = target.real, true
	dir, err := os.Open(filepath.Dir(target.real))
	if err != nil {
		result.Warning = "binary 已更新，但无法打开安装目录进行同步"
		return result, nil
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		result.Warning = "binary 已更新，但安装目录同步失败"
	}
	return result, nil
}

func (u updater) response(ctx context.Context, address, action string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("无法创建%s请求", action)
	}
	req.Header.Set("User-Agent", "cpagw-upgrade")
	if address == latestURL {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	resp, err := u.client.Do(req)
	if err != nil {
		// net/http 错误可能含签名下载 URL，不向用户回显原始错误或响应正文。
		return nil, transferError(ctx, action, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s失败（HTTP %d）", action, resp.StatusCode)
	}
	return resp, nil
}

func transferError(ctx context.Context, action string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s已取消：%w", action, ctx.Err())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s超时", action)
	}
	return fmt.Errorf("%s失败，请检查网络、代理、HTTPS 重定向及临时文件写权限", action)
}

func (u updater) fetch(ctx context.Context, address, action string, limit int64) ([]byte, error) {
	resp, err := u.response(ctx, address, action)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, transferError(ctx, "读取"+action+"响应", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s响应超过大小限制", action)
	}
	return data, nil
}

func (u updater) download(ctx context.Context, address string, dst io.Writer, limit int64) error {
	resp, err := u.response(ctx, address, "下载安装包")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	n, err := io.Copy(dst, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return transferError(ctx, "下载或保存安装包", err)
	}
	if n > limit {
		return fmt.Errorf("安装包超过大小限制")
	}
	return nil
}

func checksumFor(data []byte, archive string) (string, error) {
	var expected string
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimPrefix(fields[len(fields)-1], "*") == archive {
			count++
			if len(fields) != 2 || len(fields[0]) != sha256.Size*2 {
				return "", fmt.Errorf("checksums.txt 中目标安装包的 SHA256 摘要无效")
			}
			if _, err := hex.DecodeString(fields[0]); err != nil {
				return "", fmt.Errorf("checksums.txt 中目标安装包的 SHA256 摘要无效")
			}
			expected = strings.ToLower(fields[0])
		}
	}
	if count != 1 {
		return "", fmt.Errorf("checksums.txt 中缺少或重复目标安装包的记录")
	}
	return expected, nil
}

func extractBinary(src io.Reader, dst io.Writer, bounds limits) error {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("安装包不是有效的 gzip 文件")
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: gz, N: bounds.unpacked + 1}
	tr := tar.NewReader(limited)
	found := false
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("安装包归档损坏或解压大小超过限制")
		}
		name := header.Name
		if path.IsAbs(name) || strings.Contains(name, "\\") {
			return fmt.Errorf("安装包包含危险路径")
		}
		for _, segment := range strings.Split(name, "/") {
			if segment == ".." {
				return fmt.Errorf("安装包包含危险路径")
			}
		}
		if name != "cpagw" {
			continue
		}
		if found || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) {
			return fmt.Errorf("安装包中的 cpagw 必须是唯一的普通文件")
		}
		if header.Size <= 0 || header.Size > bounds.binary {
			return fmt.Errorf("安装包中的 cpagw 为空或超过大小限制")
		}
		if _, err := io.Copy(dst, tr); err != nil {
			return fmt.Errorf("无法解压或保存新 binary")
		}
		found = true
	}
	// tar 的结束标记早于 gzip 结尾，继续读至 EOF 才能验证 gzip 校验与总解压大小。
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return fmt.Errorf("安装包 gzip 结尾损坏")
	}
	if limited.N == 0 {
		return fmt.Errorf("安装包解压大小超过限制")
	}
	if !found {
		return fmt.Errorf("安装包中未找到根目录的 cpagw binary")
	}
	return nil
}

type installTarget struct {
	entry string
	real  string
	info  os.FileInfo
}

func locateTarget(executable func() (string, error)) (installTarget, error) {
	entry, err := executable()
	if err != nil {
		return installTarget{}, fmt.Errorf("无法确定当前 binary 路径")
	}
	real, err := filepath.EvalSymlinks(entry)
	if err != nil {
		return installTarget{}, fmt.Errorf("无法解析当前 binary 的真实路径")
	}
	info, err := os.Lstat(real)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return installTarget{}, fmt.Errorf("当前 binary 必须是具有执行权限的普通文件")
	}
	return installTarget{entry: entry, real: real, info: info}, nil
}

func (target installTarget) checkUnchanged() error {
	real, err := filepath.EvalSymlinks(target.entry)
	if err != nil || real != target.real {
		return fmt.Errorf("升级期间 binary 路径发生变化，已中止替换")
	}
	info, err := os.Lstat(target.real)
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(target.info, info) ||
		info.Size() != target.info.Size() || !info.ModTime().Equal(target.info.ModTime()) ||
		info.Mode() != target.info.Mode() {
		return fmt.Errorf("升级期间 binary 被修改或替换，已中止替换")
	}
	return nil
}
