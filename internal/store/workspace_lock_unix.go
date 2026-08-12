//go:build !windows

package store

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// workspaceLockFileName 是 workspace 锁文件名（锁文件本身不参与数据读写）。
const workspaceLockFileName = ".writer.lock"

// openWorkspaceLockFile 打开（必要时创建）锁文件。
func openWorkspaceLockFile(root string) (*os.File, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(root, workspaceLockFileName)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// lockWorkspaceFileExclusive 对锁文件加独占锁（flock LOCK_EX|LOCK_NB）。
// 被其他进程占用时返回 errWorkspaceLockBusy。进程退出/崩溃时 OS 自动释放。
func lockWorkspaceFileExclusive(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errWorkspaceLockBusy
	}
	return err
}
