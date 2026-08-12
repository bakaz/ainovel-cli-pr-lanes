package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// IO 封装文件系统读写操作，提供加锁和原子写入。
// 每个子存储持有独立的 IO 实例，拥有各自的 sync.RWMutex。
type IO struct {
	dir string
	mu  sync.RWMutex
	// state 是所属 Store 的统一写守卫状态（缺口 1/2）：写入口统一检查（未
	// ready / 只读 / Close 后全部拒绝）。nil = 独立 IO（无 Store 状态，
	// 不拦截——如单测直接 NewCheckpointStore(newIO(dir))）。
	state *storeState
}

func newIO(dir string) *IO {
	return &IO{dir: dir}
}

// guardWrite 是写操作统一的 fail-closed 检查（阻塞项 4 + 缺口 1/2）：未 ready
// （锁失败 / checkpoint 损坏）、只读 Store、Close 后——四类情况全部经共享
// storeState 拒绝，返回稳定错误。读操作不受影响（磁盘读取无害）。
// 注意：物理写方法应使用 beginWrite（含写生命周期 lease）；本方法仅供
// 无物理写、但需先经 guard 的入口使用（如 Runtime.Reset 的内存状态重置）。
func (io *IO) guardWrite() error {
	return io.state.writeBlocked()
}

// WriteHooks 提供测试用的 IO 写注入点。生产代码不设置。
type WriteHooks struct {
	// BeforePhysicalWrite 在物理写方法获取写生命周期 lease 并 guard 通过后、
	// 开始 OS 修改前调用。用于测试观测/阻塞在途写（Close 等待在途写测试）。
	BeforePhysicalWrite func()
}

var writeHooks WriteHooks

// SetWriteHooks 设置测试钩子（非线程安全，仅测试使用）。
func SetWriteHooks(hooks WriteHooks) { writeHooks = hooks }

// beginWrite 是物理写方法的统一入口守卫（Close 与在途写竞争窗口修复）：
//  1. 先获取写生命周期读 lease（st.lease.RLock）——在途写标记：Close() 的
//     独占 lease 必须等待本写完成，或在途写被 closed guard 拒绝；
//  2. 再检查 fail-closed guard（closed/readOnly/readyErr）。
//
// 顺序说明：guard 检查必须在 RLock 之后——若"先 guard 后 RLock"，通过 guard
// 但尚未获 RLock 的写会被 Close 的 Lock→Swap→释放锁→Unlock 越过，随后仍
// 落盘（窗口未闭合）。RLock 先行使：已获 RLock 的在途写被 Close 等待；未获
// RLock 的写在 Close 完成后经 closed guard 拒绝。
//
// 调用方必须在完整 OS 修改完成后调用返回的 endWrite（defer）。
// 锁序：调用方持有 io.mu（Unlocked 方法不变量）→ 本方法获取 lease.RLock；
// Close 的 lease.Lock 从不获取 io.mu → 无反向嵌套、无死锁环（RLock 共享，
// 多个在途写并行不互斥）。
func (io *IO) beginWrite() (endWrite func(), err error) {
	if io.state == nil {
		return func() {}, nil // 独立 IO（无 Store 状态），不拦截
	}
	io.state.lease.RLock()
	if err := io.state.writeBlocked(); err != nil {
		io.state.lease.RUnlock()
		return nil, err
	}
	if h := writeHooks.BeforePhysicalWrite; h != nil {
		h()
	}
	return io.state.lease.RUnlock, nil
}

func (io *IO) path(rel string) string {
	return filepath.Join(io.dir, rel)
}

func (io *IO) ReadFile(rel string) ([]byte, error) {
	io.mu.RLock()
	defer io.mu.RUnlock()
	return io.ReadFileUnlocked(rel)
}

func (io *IO) ReadFileUnlocked(rel string) ([]byte, error) {
	return os.ReadFile(io.path(rel))
}

func (io *IO) WriteFileUnlocked(rel string, data []byte) error {
	// 写生命周期 lease：完整 OS 修改期间持读 lease（Close 等待在途写）。
	endWrite, err := io.beginWrite()
	if err != nil {
		return err
	}
	defer endWrite()
	p := io.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), filepath.Base(p)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, p)
}

func (io *IO) ReadJSON(rel string, v any) error {
	io.mu.RLock()
	defer io.mu.RUnlock()
	return io.ReadJSONUnlocked(rel, v)
}

func (io *IO) ReadJSONUnlocked(rel string, v any) error {
	data, err := io.ReadFileUnlocked(rel)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func (io *IO) WriteJSON(rel string, v any) error {
	io.mu.Lock()
	defer io.mu.Unlock()
	return io.WriteJSONUnlocked(rel, v)
}

func (io *IO) WriteJSONUnlocked(rel string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return io.WriteFileUnlocked(rel, data)
}

func (io *IO) WriteMarkdown(rel string, content string) error {
	io.mu.Lock()
	defer io.mu.Unlock()
	return io.WriteFileUnlocked(rel, []byte(content))
}

// WriteMarkdownUnlocked 写出 .md sidecar。约定：每个 .md 都是对应 .json 的
// best-effort 人类可读视图，绝非数据源——运行时与导出一律从 .json 重新渲染。
// 各 Save 方法在同一写锁内先写 .json 再写此 .md，是两次独立的 tmp+rename；
// 二者之间崩溃会留下 .md 落后于 .json，这是可接受的（无人把 .md 当数据读，
// 下次写同一 scope 即自愈）。故意不为此加两文件原子提交——那是过度设计。
func (io *IO) WriteMarkdownUnlocked(rel string, content string) error {
	return io.WriteFileUnlocked(rel, []byte(content))
}

func (io *IO) AppendLine(rel string, data []byte) error {
	io.mu.Lock()
	defer io.mu.Unlock()
	return io.AppendLineUnlocked(rel, data)
}

func (io *IO) AppendLineUnlocked(rel string, data []byte) error {
	// 写生命周期 lease：完整 OS 修改期间持读 lease（Close 等待在途写）。
	endWrite, err := io.beginWrite()
	if err != nil {
		return err
	}
	defer endWrite()
	p := io.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err = f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

func (io *IO) RemoveFile(rel string) error {
	io.mu.Lock()
	defer io.mu.Unlock()
	return io.RemoveFileUnlocked(rel)
}

func (io *IO) RemoveFileUnlocked(rel string) error {
	// 写生命周期 lease：完整 OS 修改期间持读 lease（Close 等待在途写）。
	endWrite, err := io.beginWrite()
	if err != nil {
		return err
	}
	defer endWrite()
	err = os.Remove(io.path(rel))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// RemoveAllUnlocked 递归删除目录/文件（缺口 3：统一写守卫，替换绕过 guard 的
// 直接 os.RemoveAll；调用方须持有 io.mu 写锁）。
func (io *IO) RemoveAllUnlocked(rel string) error {
	// 写生命周期 lease：完整 OS 修改期间持读 lease（Close 等待在途写）。
	endWrite, err := io.beginWrite()
	if err != nil {
		return err
	}
	defer endWrite()
	return os.RemoveAll(io.path(rel))
}

func (io *IO) WithWriteLock(fn func() error) error {
	io.mu.Lock()
	defer io.mu.Unlock()
	return fn()
}

// EnsureDirs 创建指定的子目录。
func (io *IO) EnsureDirs(dirs []string) error {
	// 写生命周期 lease：完整 OS 修改期间持读 lease（Close 等待在途写）。
	endWrite, err := io.beginWrite()
	if err != nil {
		return err
	}
	defer endWrite()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(io.dir, d), 0o755); err != nil {
			return fmt.Errorf("create dir %s: %w", d, err)
		}
	}
	return nil
}
