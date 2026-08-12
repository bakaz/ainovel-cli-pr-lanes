package headless

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/host"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── 真实 Host 集成测试（次要 3，尽力而为） ─────────────────────────────
// 用真实 host.New（profile → store → models → workers 全链路）验证：
//   - consume 在真实 Host 三通道关闭后正常返回统计与退出码；
//   - ContinueAndWait 走真实 arbiter LLM 调用（不可达 provider → 干预失败 → 退出码 2）。
// 模型 stub：ollama provider 指向不可达端口——构造不联网，LLM 调用立即连接拒绝。

// stubHostConfig 构造真实 Host 可用的最小配置。
func stubHostConfig(dir string) bootstrap.Config {
	return bootstrap.Config{
		Provider:  "ollama",
		ModelName: "dummy-model",
		Style:     "default",
		OutputDir: dir,
		Providers: map[string]bootstrap.ProviderConfig{
			"ollama": {BaseURL: "http://127.0.0.1:1"},
		},
	}
}

// newRealHost 构造真实 Host；调用方负责 Close（Close 幂等，可安全双调用）。
func newRealHost(t *testing.T, dir string) *host.Host {
	t.Helper()
	eng, err := host.New(stubHostConfig(dir), assets.Load("default", assets.LoadOptions{}))
	if err != nil {
		t.Fatalf("host.New: %v", err)
	}
	return eng
}

// TestConsume_RealHost_ClosedClean 验证真实 Host 直接关闭后 consume 立即返回：
// 三通道关闭（host.Close 会 close events/stream/done），无 error 事件且状态
// 文件齐全 → 干净完成（nil + errorEvents=0）。
func TestConsume_RealHost_ClosedClean(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	// 复核阻塞项 2（方案 A）：同一 workspace 只允许一个可写 Store——铺完种子
	// 状态后释放，Host 内部 store 才能获取锁（种子已持久化在磁盘）。
	st.Close()
	eng := newRealHost(t, dir)
	eng.Close() // 不启动引擎：直接关闭

	stats, err := consume(eng, &strings.Builder{}, &strings.Builder{}, false)
	if err != nil {
		t.Fatalf("clean close must yield clean recovery: %v", err)
	}
	if stats.errorEvents != 0 {
		t.Fatalf("errorEvents = %d, want 0", stats.errorEvents)
	}
}

// TestConsume_RealHost_PendingRewritesExit5 验证真实 Host 关闭 + 磁盘存在
// 未排空重写队列 → ExitCodeError{5}（恢复未完成）。
func TestConsume_RealHost_PendingRewritesExit5(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "打磨"); err != nil {
		t.Fatal(err)
	}
	// 复核阻塞项 2（方案 A）：铺完种子状态后释放，Host 内部 store 才能取锁。
	st.Close()
	eng := newRealHost(t, dir)
	eng.Close()

	_, err := consume(eng, &strings.Builder{}, &strings.Builder{}, false)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 5 {
		t.Fatalf("expected ExitCodeError{5} on undrained rewrites, got %v", err)
	}
}

// TestRunContinue_RealHost_ArbiterFailureExit2 验证真实 ContinueAndWait 链路：
// 干预裁定走真实 arbiter LLM 调用 → stub provider 返回 401（认证错误，客户端
// 不重试，立即失败）→ 干预失败 → 退出码 2（真实调用失败路径，非假实现注入）。
func TestRunContinue_RealHost_ArbiterFailureExit2(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"invalid api key","type":"auth_error"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	cfg := stubHostConfig(dir)
	cfg.Providers["ollama"] = bootstrap.ProviderConfig{BaseURL: srv.URL}
	// 复核阻塞项 2（方案 A）：铺完种子状态后释放，Host 内部 store 才能取锁。
	st.Close()
	eng, err := host.New(cfg, assets.Load("default", assets.LoadOptions{}))
	if err != nil {
		t.Fatalf("host.New: %v", err)
	}
	defer eng.Close()

	var stderr strings.Builder
	err = runContinue(eng, &strings.Builder{}, &stderr, "继续")
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 2 {
		t.Fatalf("arbiter LLM failure must map to ExitCodeError{2}, got %v", err)
	}
}
