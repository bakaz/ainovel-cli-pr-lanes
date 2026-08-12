//go:build windows

package store

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// workspaceLockFileName 是 workspace 锁文件名（锁文件本身不参与数据读写）。
const workspaceLockFileName = ".writer.lock"

// openWorkspaceLockFile 打开（必要时创建）锁文件。
// 显式使用 CreateFile + FILE_SHARE_DELETE。说明（复核建议项）：保留删除共享
// 是有意为之——(1) 删除共享不会造成双写窗口：文件被标记 delete-pending 期间，
// 新打开同一路径会失败（ERROR_ACCESS_DENIED），锁仍排他，进程退出后文件才真正
// 消失；(2) 移除删除共享会让测试的 t.TempDir() 清理（os.RemoveAll）在锁句柄
// 仍打开时失败（Windows ERROR_SHARING_VIOLATION，已实测），而测试无法在
// 数百个 NewStore 调用点逐一补 Close（"只改下列相关文件"约束）。
func openWorkspaceLockFile(root string) (*os.File, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	p, err := windows.UTF16PtrFromString(filepath.Join(root, workspaceLockFileName))
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), workspaceLockFileName), nil
}

// lockWorkspaceFileExclusive 对锁文件加独占锁（LockFileEx，非阻塞）。
// 锁 1 字节区间；被其他进程占用时返回 errWorkspaceLockBusy。
// 进程退出/崩溃时 OS 自动释放句柄锁。
func lockWorkspaceFileExclusive(f *os.File) error {
	var ol windows.Overlapped
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &ol,
	)
	if err == windows.ERROR_LOCK_VIOLATION || err == windows.ERROR_IO_PENDING {
		// FAIL_IMMEDIATELY 下被占用即返回 ERROR_LOCK_VIOLATION；
		// ERROR_IO_PENDING 理论不可达，防御性同样视为占用。
		return errWorkspaceLockBusy
	}
	return err
}
