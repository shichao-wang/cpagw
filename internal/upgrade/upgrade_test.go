package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testVersion = "v2026.10.5-abc123"
const testArchive = "cpagw_darwin_arm64.tar.gz"

type tarEntry struct {
	name string
	body string
	kind byte
}

func makeArchive(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Mode: 0755, Typeflag: kind}
		if kind == tar.TypeReg {
			header.Size = int64(len(entry.body))
		} else if kind == tar.TypeSymlink || kind == tar.TypeLink {
			header.Linkname = "other"
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := io.WriteString(tw, entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func releaseJSON(t *testing.T, tag string, assets ...string) []byte {
	t.Helper()
	var list []map[string]string
	for _, asset := range assets {
		list = append(list, map[string]string{"name": asset, "browser_download_url": "https://untrusted.invalid/ignored"})
	}
	data, err := json.Marshal(map[string]any{"tag_name": tag, "draft": false, "prerelease": false, "assets": list})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func archiveChecksum(data []byte) []byte {
	hash := sha256.Sum256(data)
	return []byte(hex.EncodeToString(hash[:]) + "  " + testArchive + "\n")
}

type rewriteTransport struct {
	base   http.RoundTripper
	server *url.URL
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	address := *req.URL
	address.Scheme, address.Host = r.server.Scheme, r.server.Host
	clone.URL = &address
	return r.base.RoundTrip(clone)
}

type fixture struct {
	u         updater
	dir       string
	binary    string
	metadata  []byte
	archive   []byte
	checksums []byte
	status    map[string]int
	hook      func(http.ResponseWriter, *http.Request) bool
	mu        sync.Mutex
	requests  []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{u: newUpdater(), dir: t.TempDir(), status: make(map[string]int)}
	f.binary = filepath.Join(f.dir, "cpagw")
	if err := os.WriteFile(f.binary, []byte("旧 binary"), 0751); err != nil {
		t.Fatal(err)
	}
	f.archive = makeArchive(t, tarEntry{name: "cpagw", body: "新 binary"}, tarEntry{name: "README.md", body: "说明"})
	f.checksums = archiveChecksum(f.archive)
	f.metadata = releaseJSON(t, testVersion, testArchive, "checksums.txt")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.URL.Path)
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "" {
			t.Error("升级不应发送 Authorization")
		}
		if f.hook != nil && f.hook(w, r) {
			return
		}
		if status := f.status[r.URL.Path]; status != 0 {
			w.WriteHeader(status)
			fmt.Fprint(w, "不应回显的响应正文 sentinel-secret")
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			w.Write(f.metadata)
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			w.Write(f.checksums)
		case strings.HasSuffix(r.URL.Path, "/"+testArchive):
			w.Write(f.archive)
		default:
			t.Errorf("意外请求路径：%s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	f.u.client.Transport = rewriteTransport{base: server.Client().Transport, server: address}
	f.u.goos, f.u.goarch = "darwin", "arm64"
	f.u.executable = func() (string, error) { return f.binary, nil }
	return f
}

func (f *fixture) assertContents(t *testing.T, want string) {
	t.Helper()
	data, err := os.ReadFile(f.binary)
	if err != nil || string(data) != want {
		t.Fatalf("binary 内容错误：%q，错误：%v", data, err)
	}
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".cpagw-upgrade-") {
			t.Errorf("临时目录未清理：%s", entry.Name())
		}
	}
}

func TestArchiveName(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		for _, goarch := range []string{"amd64", "arm64"} {
			name, err := archiveName(goos, goarch)
			want := "cpagw_" + goos + "_" + goarch + ".tar.gz"
			if err != nil || name != want {
				t.Fatalf("平台映射错误：%s/%s：%q，%v", goos, goarch, name, err)
			}
		}
	}
	for _, platform := range [][2]string{{"windows", "amd64"}, {"linux", "386"}, {"freebsd", "arm64"}} {
		if _, err := archiveName(platform[0], platform[1]); err == nil {
			t.Fatalf("应拒绝不支持的平台：%v", platform)
		}
	}
}

func TestUpgradeSuccess(t *testing.T) {
	for _, current := range []string{"v2026.10.4-older", "", "v2026.10.5-zzz999"} {
		t.Run("当前版本="+current, func(t *testing.T) {
			f := newFixture(t)
			settings := filepath.Join(f.dir, "settings.json")
			if err := os.WriteFile(settings, []byte("配置哨兵"), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := f.u.run(context.Background(), current)
			if err != nil || !result.Updated || result.Version != testVersion || result.Warning != "" {
				t.Fatalf("升级失败：%+v，%v", result, err)
			}
			real, err := filepath.EvalSymlinks(f.binary)
			if err != nil || result.Path != real {
				t.Fatalf("安装路径错误：%q，%v", result.Path, err)
			}
			f.assertContents(t, "新 binary")
			info, err := os.Stat(f.binary)
			if err != nil || info.Mode().Perm() != 0751 {
				t.Fatalf("未保留执行权限：%v，%v", info, err)
			}
			data, err := os.ReadFile(settings)
			if err != nil || string(data) != "配置哨兵" {
				t.Fatalf("无关配置被修改：%q，%v", data, err)
			}
			if _, err := os.Stat(filepath.Join(f.dir, "README.md")); !os.IsNotExist(err) {
				t.Fatalf("不应解包其他文件：%v", err)
			}
		})
	}
}

func TestAlreadyLatest(t *testing.T) {
	f := newFixture(t)
	f.u.executable = func() (string, error) {
		t.Error("同版本不应解析或修改安装路径")
		return "", errors.New("不应调用")
	}
	result, err := f.u.run(context.Background(), testVersion)
	if err != nil || result.Updated || result.Version != testVersion {
		t.Fatalf("同版本应无操作：%+v，%v", result, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 {
		t.Fatalf("同版本应只查询 metadata：%v", f.requests)
	}
	f.assertContents(t, "旧 binary")
}

func TestUpgradeSymlink(t *testing.T) {
	f := newFixture(t)
	link := filepath.Join(f.dir, "cpagw-link")
	if err := os.Symlink("cpagw", link); err != nil {
		t.Fatal(err)
	}
	f.u.executable = func() (string, error) { return link, nil }
	result, err := f.u.run(context.Background(), "")
	if err != nil || !result.Updated {
		t.Fatalf("通过 symlink 升级失败：%+v，%v", result, err)
	}
	if value, err := os.Readlink(link); err != nil || value != "cpagw" {
		t.Fatalf("symlink 被替换：%q，%v", value, err)
	}
	f.assertContents(t, "新 binary")
}

func TestInvalidRelease(t *testing.T) {
	cases := map[string][]byte{
		"无效JSON":     []byte("{broken"),
		"空tag":       releaseJSON(t, "", testArchive, "checksums.txt"),
		"危险tag":      releaseJSON(t, "../escape", testArchive, "checksums.txt"),
		"缺少资产":       releaseJSON(t, testVersion, "checksums.txt"),
		"重复资产":       releaseJSON(t, testVersion, testArchive, testArchive, "checksums.txt"),
		"缺少checksum": releaseJSON(t, testVersion, testArchive),
		"重复checksum": releaseJSON(t, testVersion, testArchive, "checksums.txt", "checksums.txt"),
		"草稿":         []byte(`{"tag_name":"v1","draft":true}`),
		"预发布":        []byte(`{"tag_name":"v1","prerelease":true}`),
	}
	for name, metadata := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.metadata = metadata
			if _, err := f.u.run(context.Background(), ""); err == nil {
				t.Fatal("应拒绝无效 Release")
			}
			f.assertContents(t, "旧 binary")
		})
	}
}

func TestChecksumFor(t *testing.T) {
	digest := strings.Repeat("a", 64)
	cases := []struct {
		name string
		data string
		ok   bool
	}{
		{"标准格式", digest + "  " + testArchive + "\n", true},
		{"空白与大写", strings.ToUpper(digest) + "\t" + testArchive + "\r\n", true},
		{"二进制标记", digest + " *" + testArchive, true},
		{"其他记录", "其他不匹配内容\n" + digest + "  " + testArchive, true},
		{"缺失", digest + "  unrelated.tar.gz", false},
		{"重复", strings.Repeat(digest+"  "+testArchive+"\n", 2), false},
		{"摘要过短", "abcd  " + testArchive, false},
		{"摘要非hex", strings.Repeat("z", 64) + "  " + testArchive, false},
		{"额外字段", digest + " other " + testArchive, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := checksumFor([]byte(tc.data), testArchive)
			if (err == nil) != tc.ok || (tc.ok && got != digest) {
				t.Fatalf("校验记录解析错误：%q，%v", got, err)
			}
		})
	}
}

func TestUpgradeFailurePreservesBinary(t *testing.T) {
	cases := map[string]func(*fixture){
		"checksum缺失": func(f *fixture) { f.checksums = nil },
		"checksum重复": func(f *fixture) { f.checksums = append(f.checksums, f.checksums...) },
		"checksum错误": func(f *fixture) {
			f.checksums = []byte(strings.Repeat("0", 64) + "  " + testArchive)
		},
		"metadata超限": func(f *fixture) { f.u.limits.metadata = 10 },
		"checksum超限": func(f *fixture) { f.u.limits.checksum = 10 },
		"archive超限":  func(f *fixture) { f.u.limits.archive = 10 },
		"解压超限":       func(f *fixture) { f.u.limits.unpacked = 520 },
		"binary超限":   func(f *fixture) { f.u.limits.binary = 2 },
		"无执行权限": func(f *fixture) {
			if err := os.Chmod(f.binary, 0600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			prepare(f)
			if _, err := f.u.run(context.Background(), ""); err == nil {
				t.Fatal("应中止升级")
			}
			f.assertContents(t, "旧 binary")
		})
	}
}

func TestInvalidArchives(t *testing.T) {
	valid := tarEntry{name: "cpagw", body: "新 binary"}
	cases := map[string][]byte{
		"不是gzip":   []byte("not gzip"),
		"缺少binary": makeArchive(t, tarEntry{name: "README.md", body: "说明"}),
		"重复binary": makeArchive(t, valid, valid),
		"symlink":  makeArchive(t, tarEntry{name: "cpagw", kind: tar.TypeSymlink}),
		"hardlink": makeArchive(t, tarEntry{name: "cpagw", kind: tar.TypeLink}),
		"目录":       makeArchive(t, tarEntry{name: "cpagw", kind: tar.TypeDir}),
		"嵌套binary": makeArchive(t, tarEntry{name: "nested/cpagw", body: "新 binary"}),
		"空binary":  makeArchive(t, tarEntry{name: "cpagw"}),
		"穿越路径":     makeArchive(t, valid, tarEntry{name: "../escape", body: "不能落盘"}),
		"绝对路径":     makeArchive(t, valid, tarEntry{name: "/escape", body: "不能落盘"}),
	}
	corrupt := makeArchive(t, valid)
	corrupt[len(corrupt)-8] ^= 0xff
	cases["gzip校验错误"] = corrupt
	truncated := makeArchive(t, valid)
	cases["gzip截断"] = truncated[:len(truncated)-5]
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "cpagw", Size: 100, Mode: 0755}); err != nil {
		t.Fatal(err)
	}
	// 故意不补全 tar entry，仅完成 gzip 封装。
	if _, err := tw.Write([]byte("短内容")); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	cases["tar截断"] = raw.Bytes()
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.archive, f.checksums = data, archiveChecksum(data)
			if _, err := f.u.run(context.Background(), ""); err == nil {
				t.Fatal("应拒绝异常归档")
			}
			f.assertContents(t, "旧 binary")
		})
	}
}

func TestHTTPFailures(t *testing.T) {
	for _, asset := range []string{"latest", "checksums.txt", testArchive} {
		for _, status := range []int{http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError} {
			t.Run(fmt.Sprintf("%s-%d", asset, status), func(t *testing.T) {
				f := newFixture(t)
				address := downloadURL + testVersion + "/" + asset
				if asset == "latest" {
					address = latestURL
				}
				u, err := url.Parse(address)
				if err != nil {
					t.Fatal(err)
				}
				f.status[u.Path] = status
				_, err = f.u.run(context.Background(), "")
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) || strings.Contains(err.Error(), "sentinel-secret") {
					t.Fatalf("HTTP 错误处理不正确：%v", err)
				}
				f.assertContents(t, "旧 binary")
			})
		}
	}
}

func TestRedirects(t *testing.T) {
	for _, scheme := range []string{"https", "http"} {
		t.Run(scheme, func(t *testing.T) {
			f := newFixture(t)
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasSuffix(r.URL.Path, "/"+testArchive) {
					http.Redirect(w, r, scheme+"://release-assets.githubusercontent.com/redirected", http.StatusFound)
					return true
				}
				if r.URL.Path == "/redirected" {
					w.Write(f.archive)
					return true
				}
				return false
			}
			result, err := f.u.run(context.Background(), "")
			if scheme == "https" {
				if err != nil || !result.Updated {
					t.Fatalf("应允许 HTTPS 资产重定向：%+v，%v", result, err)
				}
				f.assertContents(t, "新 binary")
			} else {
				if err == nil {
					t.Fatal("应拒绝 HTTP 降级")
				}
				f.mu.Lock()
				for _, request := range f.requests {
					if request == "/redirected" {
						t.Error("HTTP 降级请求不应发出")
					}
				}
				f.mu.Unlock()
				f.assertContents(t, "旧 binary")
			}
		})
	}
}

func TestCancellationAndTimeout(t *testing.T) {
	t.Run("请求前取消", func(t *testing.T) {
		f := newFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := f.u.run(ctx, ""); !errors.Is(err, context.Canceled) {
			t.Fatalf("应保留取消原因：%v", err)
		}
		f.assertContents(t, "旧 binary")
	})
	t.Run("提交前取消", func(t *testing.T) {
		f := newFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.hook = func(w http.ResponseWriter, r *http.Request) bool {
			if strings.HasSuffix(r.URL.Path, "/"+testArchive) {
				w.Write(f.archive)
				cancel()
				return true
			}
			return false
		}
		if _, err := f.u.run(ctx, ""); err == nil {
			t.Fatal("取消后不应提交替换")
		}
		f.assertContents(t, "旧 binary")
	})
	t.Run("请求超时", func(t *testing.T) {
		f := newFixture(t)
		f.u.client.Timeout = 50 * time.Millisecond
		f.hook = func(w http.ResponseWriter, r *http.Request) bool {
			<-r.Context().Done()
			return true
		}
		if _, err := f.u.run(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "超时") {
			t.Fatalf("应报告超时：%v", err)
		}
		f.assertContents(t, "旧 binary")
	})
}

func TestResponseBodyCancellationAndTimeout(t *testing.T) {
	for _, asset := range []string{"latest", "checksums.txt", testArchive} {
		for _, timeout := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-超时=%t", asset, timeout), func(t *testing.T) {
				f := newFixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				started := make(chan struct{})
				release := make(chan struct{})
				var releaseOnce sync.Once
				defer releaseOnce.Do(func() { close(release) })
				f.hook = func(w http.ResponseWriter, r *http.Request) bool {
					if !strings.HasSuffix(r.URL.Path, "/"+asset) {
						return false
					}
					w.Write([]byte("partial"))
					w.(http.Flusher).Flush()
					close(started)
					// 等待客户端先完成取消或超时断言，避免响应结束和读取错误竞态。
					<-release
					return true
				}
				if timeout {
					f.u.client.Timeout = 250 * time.Millisecond
				}
				done := make(chan error, 1)
				go func() {
					_, err := f.u.run(ctx, "")
					done <- err
				}()
				select {
				case <-started:
				case err := <-done:
					t.Fatalf("响应体阶段开始前请求已失败：%v", err)
				}
				if !timeout {
					cancel()
				}
				err := <-done
				releaseOnce.Do(func() { close(release) })
				if timeout {
					if err == nil || !strings.Contains(err.Error(), "超时") {
						t.Fatalf("响应读取中超时应清楚报告：%v", err)
					}
				} else if !errors.Is(err, context.Canceled) {
					t.Fatalf("响应读取中取消应保留取消原因：%v", err)
				}
				f.assertContents(t, "旧 binary")
			})
		}
	}
}

func TestChangedTarget(t *testing.T) {
	for _, change := range []string{"替换", "原地修改", "symlink改向"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			link := filepath.Join(f.dir, "entry")
			if err := os.Symlink("cpagw", link); err != nil {
				t.Fatal(err)
			}
			f.u.executable = func() (string, error) { return link, nil }
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/"+testArchive) {
					return false
				}
				switch change {
				case "替换":
					temp := filepath.Join(f.dir, "other")
					if err := os.WriteFile(temp, []byte("其他更新"), 0751); err != nil {
						t.Error(err)
					}
					if err := os.Rename(temp, f.binary); err != nil {
						t.Error(err)
					}
				case "原地修改":
					if err := os.WriteFile(f.binary, []byte("其他更新"), 0751); err != nil {
						t.Error(err)
					}
				case "symlink改向":
					if err := os.Remove(link); err != nil {
						t.Error(err)
					}
					if err := os.Symlink("missing", link); err != nil {
						t.Error(err)
					}
				}
				return false
			}
			if _, err := f.u.run(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "升级期间") {
				t.Fatalf("应拒绝覆盖变化后的目标：%v", err)
			}
			want := "其他更新"
			if change == "symlink改向" {
				want = "旧 binary"
			}
			f.assertContents(t, want)
		})
	}
}

func TestUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受普通目录写权限限制")
	}
	f := newFixture(t)
	if err := os.Chmod(f.dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(f.dir, 0700) })
	if _, err := f.u.run(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "写权限") {
		t.Fatalf("目录不可写时应失败而不是换安装路径：%v", err)
	}
	f.assertContents(t, "旧 binary")
}
