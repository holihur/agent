// Package selfupdate 让 agent CLI 具备自我升级能力:
// 检查 GitHub 最新 release,下载对应平台归档,校验 sha256 后原子替换当前二进制。
// 逻辑与 install.sh 保持一致(归档命名 agent_OS_ARCH.tar.gz + checksums.txt)。
package selfupdate

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Options 一次自升级的配置;零值默认 holihur/agent + https://github.com。
type Options struct {
	Repo       string // OWNER/NAME(默认 holihur/agent)
	GitHubBase string // GitHub base URL(默认 https://github.com,可指向代理/测试服务器)
	Version    string // 当前版本(ldflags 注入;"dev" 或空 = 直接升级到最新)
	Executable string // 要替换的二进制路径(默认 os.Executable())
	Client     *http.Client
	Progress   func(msg string) // 进度输出;nil = 静默
}

// Result 描述一次自升级的执行结果。
type Result struct {
	Latest     string // 最新 release tag
	Updated    bool   // 是否实际替换了二进制
	InstallTo  string // 二进制落点(替换原路径,或 ~/.local/bin 兜底)
	FromRemote bool   // 远端是否存在 release(用于 "已是最新" 判定)
}

// ErrUnsupportedOS 当前平台不在发布矩阵内(linux/darwin)。
var ErrUnsupportedOS = errors.New("selfupdate: unsupported OS (only linux/darwin releases are supported)")

// Latest 解析 OWNER/REPO 的最新 release tag:
// GET <base>/<repo>/releases/latest,取 302 Location 的最后一段(与 install.sh 相同,无需 API)。
func Latest(ctx context.Context, repo string) (string, error) {
	return LatestWithBase(ctx, defaultBase(), repo, nil)
}

// LatestWithBase 同 Latest,但可自定义 base URL 与 HTTP client(测试/代理用)。
func LatestWithBase(ctx context.Context, base, repo string, client *http.Client) (string, error) {
	base = strings.TrimRight(base, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	c := client
	if c == nil {
		c = http.DefaultClient
	}
	c = &http.Client{Transport: c.Transport, Timeout: c.Timeout,
		// 不跟随跳转,直接读 Location 头解析 tag
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("selfupdate: cannot resolve latest release for %s (HTTP %d)", repo, resp.StatusCode)
	}
	tag := loc[strings.LastIndex(loc, "/")+1:]
	if tag == "" || tag == "latest" {
		return "", fmt.Errorf("selfupdate: no published release found at %s/%s", base, repo)
	}
	return tag, nil
}

// Run 执行完整的自升级流程:定位最新版 → 对比 → 下载归档与校验和 →
// sha256 校验 → 解压出 agent 二进制 → 原子替换当前可执行文件。
func Run(ctx context.Context, opt Options) (Result, error) {
	var res Result
	repo := opt.Repo
	if repo == "" {
		repo = "holihur/agent"
	}
	base := opt.GitHubBase
	if base == "" {
		base = "https://github.com"
	}
	base = strings.TrimRight(base, "/")
	client := opt.Client
	if client == nil {
		client = http.DefaultClient
	}
	say := opt.Progress

	asset, err := archiveName()
	if err != nil {
		return res, err
	}
	res.Latest, err = LatestWithBase(ctx, base, repo, client)
	if err != nil {
		return res, err
	}
	res.FromRemote = true

	if !isNewer(opt.Version, res.Latest) {
		if say != nil {
			say(fmt.Sprintf("==> already up to date (%s >= %s)", opt.Version, res.Latest))
		}
		return res, nil
	}
	if say != nil {
		say(fmt.Sprintf("==> %s -> %s", opt.Version, res.Latest))
	}

	tmp, err := os.MkdirTemp("", "agent-selfupdate-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)
	urlBase := fmt.Sprintf("%s/%s/releases/download/%s", base, repo, res.Latest)

	archive := filepath.Join(tmp, asset)
	if err := download(ctx, client, urlBase+"/"+asset, archive); err != nil {
		return res, fmt.Errorf("selfupdate: %w (no asset for %s/%s?)", err, runtime.GOOS, runtime.GOARCH)
	}
	sums := filepath.Join(tmp, "checksums.txt")
	if err := download(ctx, client, urlBase+"/checksums.txt", sums); err != nil {
		return res, fmt.Errorf("selfupdate: %w", err)
	}
	if say != nil {
		say("==> verifying checksum")
	}
	if err := verifyChecksum(archive, sums, asset); err != nil {
		return res, err
	}
	if say != nil {
		say("==> checksum ok")
	}

	bin, err := extractAgent(archive, tmp)
	if err != nil {
		return res, err
	}

	exe := opt.Executable
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return res, err
		}
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return res, err
	}
	if err = replaceBinary(bin, exe); err != nil {
		// 目标目录不可写时兜底安装到 ~/.local/bin(install.sh 同款策略)
		home, herr := os.UserHomeDir()
		if herr != nil {
			return res, err
		}
		fallback := filepath.Join(home, ".local", "bin", "agent")
		if ferr := replaceBinary(bin, fallback); ferr != nil {
			return res, fmt.Errorf("selfupdate: %v (fallback %v)", err, ferr)
		}
		if say != nil {
			say(fmt.Sprintf("==> note: %s is not writable; installed to %s", exe, fallback))
		}
		exe = fallback
	}
	res.Updated = true
	res.InstallTo = exe
	if say != nil {
		say(fmt.Sprintf("==> updated to %s at %s", res.Latest, exe))
	}
	return res, nil
}

// archiveName 按 goreleaser 归档命名生成当前平台资产名:
// agent_<Title OS>_<x86_64|i386|arm64>.tar.gz(linux/darwin)。
func archiveName() (string, error) {
	osName, ok := map[string]string{"linux": "Linux", "darwin": "Darwin"}[runtime.GOOS]
	if !ok {
		return "", ErrUnsupportedOS
	}
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "386":
		arch = "i386"
	}
	return fmt.Sprintf("agent_%s_%s.tar.gz", osName, arch), nil
}

// download 下载 url 到 out 文件(覆盖写)。
func download(ctx context.Context, client *http.Client, url, out string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// verifyChecksum 计算 archive 的 sha256,与 checksums.txt 中对应资产的记录比对。
func verifyChecksum(archive, sums, asset string) error {
	data, err := os.ReadFile(sums)
	if err != nil {
		return fmt.Errorf("selfupdate: read checksums.txt: %w", err)
	}
	want := ""
	for _, line := range strings.FieldsFunc(string(data), func(r rune) bool { return r == '\n' || r == '\r' }) {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("selfupdate: checksums.txt has no entry for %s", asset)
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("selfupdate: checksum mismatch: want %s, got %s", want, got)
	}
	return nil
}

// extractAgent 从 tar.gz 归档解出名为 agent 的二进制,写到 dir/agent。
func extractAgent(archive, dir string) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("selfupdate: archive does not contain the agent binary")
		}
		if err != nil {
			return "", err
		}
		name := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./")
		if name != "agent" {
			continue
		}
		out := filepath.Join(dir, "agent")
		of, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return "", err
		}
		if _, err = io.Copy(of, tr); err != nil {
			of.Close()
			return "", err
		}
		if err = of.Close(); err != nil {
			return "", err
		}
		return out, nil
	}
}

// replaceBinary 原子替换 dst:先落临时文件再 rename,失败清理。
func replaceBinary(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".agent-update-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := os.Chmod(tmpName, 0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := copyFile(src, tmpName); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func defaultBase() string { return "https://github.com" }

// isNewer 比较 current 与 latest 两个版本号(latest 需形如 vX.Y.Z,可带前缀):
// current 为空或 "dev"(本地构建)时总是返回 true;版本相等返回 false。
func isNewer(current, latest string) bool {
	c := strings.TrimPrefix(strings.TrimSpace(current), "v")
	if c == "" || c == "dev" {
		return true
	}
	return compareSemver(c, strings.TrimPrefix(strings.TrimSpace(latest), "v")) < 0
}

// compareSemver 按 '.' 分段的数字逐段比较;非数字段按字符串比较,缺段视为 0。
func compareSemver(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := range max(len(as), len(bs)) {
		sa, sb := "0", "0"
		if i < len(as) {
			sa = as[i]
		}
		if i < len(bs) {
			sb = bs[i]
		}
		na, ea := strconv.Atoi(sa)
		nb, eb := strconv.Atoi(sb)
		switch {
		case ea == nil && eb == nil:
			if na != nb {
				return sign(na - nb)
			}
		default:
			if sa != sb {
				return strings.Compare(sa, sb)
			}
		}
	}
	return 0
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
