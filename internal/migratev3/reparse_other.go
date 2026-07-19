//go:build !windows

package migratev3

import "os"

func isReparse(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink != 0
}
