package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ── Workspace 跨进程排他锁（P0-1 + 复核阻塞项 1/2）─────────────────────
//
// 目标：防止多个 ainovel-cli 进程并发写同一 workspace（novel 目录）导致
// checkpoint seq 重复、草稿互相覆盖等数据损坏；并防止同一进程内多个 Store
// 实例（各自独立 mutex/cache）并发写同一 workspace。
// 锁文件：<root>/.writer.lock，锁文件本身不参与任何数据读写。
//
// 实现：进程内注册表（同一 workspace 路径只允许一个可写 Store）+ 平台独占锁
// （Windows LockFileEx / Unix flock(LOCK_EX|LOCK_NB)）。
//   - 同一进程内第二个可写 Store（同一路径）构造即失败——两个实例的
//     crossMu/IO mutex/cache 相互独立，可同时读相同磁盘尾部并分配相同新 seq
//     （复核阻塞项 2，方案 A）。只读用途（diag 导出/诊断/采集）用
//     NewReadOnlyStore，不取锁、不阻挡真实写入进程。
//   - 跨进程占用返回明确错误："workspace 已被另一个 ainovel-cli 实例占用"。
//   - 进程退出/崩溃时 OS 自动释放句柄锁，无需显式清理。
//   - release() 用于优雅退出（Store.Close），同时从注册表移除（同进程后续
//     可重新获取——如测试中"先铺状态再开 Host"的顺序模式）。

// errWorkspaceLockBusy 是平台锁被占用的内部哨兵（跨进程冲突）。
var errWorkspaceLockBusy = errors.New("workspace lock held by another process")

// workspaceLockEntry 记录一把已获取的 OS 锁（进程内同一路径至多一条）。
type workspaceLockEntry struct {
	file    *os.File
	release func()
}

var (
	workspaceLocksMu sync.Mutex
	workspaceLocks   = make(map[string]*workspaceLockEntry)
)

// AcquireWorkspaceLock 获取 workspace（root 为 store 数据目录，即 novel 目录）
// 的跨进程排他锁。成功返回 release 函数（Store.Close 时调用，释放 OS 锁并从
// 注册表移除）。
// 失败（明确错误，调用方应 fail-closed 拒绝写盘）：
//   - 本进程内已有可写 Store 持有同一 workspace："workspace 已被本进程内另一个
//     Store 实例占用：<root>"（复核阻塞项 2 方案 A）
//   - 另一进程占用："workspace 已被另一个 ainovel-cli 实例占用：<root>"
func AcquireWorkspaceLock(root string) (release func(), err error) {
	key := filepath.Clean(root)

	workspaceLocksMu.Lock()
	defer workspaceLocksMu.Unlock()

	if _, ok := workspaceLocks[key]; ok {
		// 复核阻塞项 2（方案 A）：同一进程内只允许一个可写 Store——第二个实例
		// 的独立 mutex/cache 会并发分配重复 seq，必须拒绝（只读场景用
		// NewReadOnlyStore，不取锁）。
		return nil, fmt.Errorf("workspace 已被本进程内另一个 Store 实例占用：%s", root)
	}

	f, err := openWorkspaceLockFile(root)
	if err != nil {
		return nil, fmt.Errorf("acquire workspace lock for %s: %w", root, err)
	}
	if err := lockWorkspaceFileExclusive(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errWorkspaceLockBusy) {
			return nil, fmt.Errorf("workspace 已被另一个 ainovel-cli 实例占用：%s", root)
		}
		return nil, fmt.Errorf("acquire workspace lock for %s: %w", root, err)
	}

	e := &workspaceLockEntry{file: f}
	e.release = func() {
		workspaceLocksMu.Lock()
		defer workspaceLocksMu.Unlock()
		if workspaceLocks[key] != e {
			return // 已释放（幂等）
		}
		_ = e.file.Close()
		delete(workspaceLocks, key)
	}
	workspaceLocks[key] = e
	return e.release, nil
}
