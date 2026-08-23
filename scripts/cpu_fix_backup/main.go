package main

import (
	"fmt"
	"os"

	"github.com/voocel/ainovel-cli/internal/backup"
	"github.com/voocel/ainovel-cli/internal/store"
)

func main() {
	src := `G:\opencode\ainovel-cli_0.6.3_Windows_x86_64\workspace\output\novel`
	release, err := store.AcquireWorkspaceLock(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lock: %v\n", err)
		os.Exit(1)
	}
	defer release()

	m, err := backup.Backup(src, "永久的乳铐", backup.KindVolume, 37, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup: %v\n", err)
		os.Exit(1)
	}
	root, _ := backup.BackupRoot(src)
	final := fmt.Sprintf("%s\\%s\\%s", root, m.Kind, m.SnapshotID)
	if _, err := backup.Verify(final); err != nil {
		fmt.Fprintf(os.Stderr, "verify: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("OK snapshot_id=%s files=%d dir=%s\n", m.SnapshotID, len(m.Files), final)
}
