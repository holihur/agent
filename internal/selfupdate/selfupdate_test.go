package selfupdate

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"dev", "v0.1.0", true},
		{"", "v0.1.0", true},
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.1.1", true},
		{"v0.1.10", "v0.1.9", false},
		{"v1.2.3", "v1.10.0", true},
		{"v0.2.0", "v0.1.9", false},
	}
	for _, c := range cases {
		if got := isNewer(c.cur, c.latest); got != c.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", c.cur, c.latest, got, c.want)
		}
	}
}

func TestCompareSemver(t *testing.T) {
	if got := compareSemver("1.2.3", "1.2.10"); got >= 0 {
		t.Errorf("compareSemver(1.2.3,1.2.10) = %d, want <0", got)
	}
	if got := compareSemver("1.2", "1.2.0"); got != 0 {
		t.Errorf("compareSemver(1.2,1.2.0) = %d, want 0", got)
	}
}

// archiveNameFor 返回与 archiveName() 相同命名规则的测试资产名。
func archiveNameFor(t *testing.T) string {
	t.Helper()
	n, err := archiveName()
	if err != nil {
		t.Skipf("unsupported test platform: %v", err)
	}
	return n
}

// newFakeGitHub 启动一个模拟 GitHub:releases/latest 跳转 + 归档/校验和下载。
func newFakeGitHub(t *testing.T, tag, asset, bin string) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256([]byte(assetContent(t, bin)))
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "agent", Mode: 0o755, Size: int64(len(bin))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(bin)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/holihur/agent/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/holihur/agent/releases/tag/"+tag)
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/holihur/agent/releases/download/"+tag+"/"+asset, func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	})
	mux.HandleFunc("/holihur/agent/releases/download/"+tag+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func assetContent(t *testing.T, bin string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "agent", Mode: 0o755, Size: int64(len(bin))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(bin)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	return buf.Bytes()
}

func TestLatestWithBase(t *testing.T) {
	srv := newFakeGitHub(t, "v1.2.3", archiveNameFor(t), "stub")
	tag, err := LatestWithBase(context.Background(), srv.URL, "holihur/agent", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v1.2.3" {
		t.Fatalf("tag = %q, want v1.2.3", tag)
	}
}

func TestRunUpdatesBinary(t *testing.T) {
	asset := archiveNameFor(t)
	srv := newFakeGitHub(t, "v9.9.9", asset, "new-binary-payload")

	exe := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	var msgs []string
	res, err := Run(context.Background(), Options{
		Repo:       "holihur/agent",
		GitHubBase: srv.URL,
		Version:    "v0.1.0",
		Executable: exe,
		Client:     srv.Client(),
		Progress:   func(msg string) { msgs = append(msgs, msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || res.Latest != "v9.9.9" || res.InstallTo != exe {
		t.Fatalf("unexpected result: %+v", res)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-binary-payload" {
		t.Fatalf("binary not replaced: %q", got)
	}
	if st, err := os.Stat(exe); err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("binary not executable: %v %v", st, err)
	}
	found := false
	for _, m := range msgs {
		if strings.Contains(m, "checksum ok") {
			found = true
		}
	}
	if !found {
		t.Fatalf("progress missing checksum step: %v", msgs)
	}
}

func TestRunAlreadyUpToDate(t *testing.T) {
	asset := archiveNameFor(t)
	srv := newFakeGitHub(t, "v1.0.0", asset, "new")
	exe := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{
		Repo:       "holihur/agent",
		GitHubBase: srv.URL,
		Version:    "v1.0.0",
		Executable: exe,
		Client:     srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated {
		t.Fatal("should not update when already latest")
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Fatal("binary should be untouched")
	}
}

func TestRunChecksumMismatch(t *testing.T) {
	asset := archiveNameFor(t)
	var inner http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "checksums.txt") {
			w.Write([]byte("0000000000000000000000000000000000000000000000000000000000000000  " + asset + "\n"))
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	inner = newFakeGitHub(t, "v9.9.9", asset, "new-binary-payload").Config.Handler
	exe := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{
		Repo:       "holihur/agent",
		GitHubBase: srv.URL,
		Version:    "dev",
		Executable: exe,
		Client:     srv.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch error, got %v", err)
	}
}

func TestRunMissingRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := Run(context.Background(), Options{Repo: "holihur/agent", GitHubBase: srv.URL, Version: "dev", Client: srv.Client()}); err == nil {
		t.Fatal("want error when no release published")
	}
}

func TestArchiveName(t *testing.T) {
	n, err := archiveName()
	if err != nil {
		t.Skipf("unsupported OS %s", runtime.GOOS)
	}
	want := "agent_" + map[string]string{"linux": "Linux", "darwin": "Darwin"}[runtime.GOOS] + "_"
	switch runtime.GOARCH {
	case "amd64":
		want += "x86_64"
	case "386":
		want += "i386"
	default:
		want += runtime.GOARCH
	}
	want += ".tar.gz"
	if n != want {
		t.Fatalf("archiveName = %q, want %q", n, want)
	}
}
