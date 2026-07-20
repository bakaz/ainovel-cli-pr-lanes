package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/voocel/ainovel-cli/internal/backup"
	"github.com/voocel/ainovel-cli/internal/host"
)

func restoreCommandError(m Model, text string) (tea.Model, tea.Cmd) {
	m.applyEvent(host.Event{Time: time.Now(), Category: "ERROR", Summary: text, Level: "error"})
	m.refreshEventViewport()
	return m, nil
}

func (m Model) listSnapshots() (tea.Model, tea.Cmd) {
	snapshots, err := m.runtime.ListSnapshots()
	if err != nil {
		return restoreCommandError(m, "Could not list snapshots: "+err.Error())
	}
	if len(snapshots) == 0 {
		m.applyEvent(host.Event{Time: time.Now(), Category: "SYSTEM", Summary: "No normal Arc/Volume snapshots.", Level: "info"})
	} else {
		for _, snapshot := range snapshots { // Host guarantees newest-first and excludes rescues.
			m.applyEvent(host.Event{Time: time.Now(), Category: "SYSTEM", Summary: formatSnapshot(snapshot), Level: "info"})
		}
	}
	m.refreshEventViewport()
	return m, nil
}

func formatSnapshot(snapshot backup.Manifest) string {
	boundary := fmt.Sprintf("Volume %d", snapshot.Volume)
	if snapshot.Kind == backup.KindArc {
		boundary += fmt.Sprintf(" Arc %d", snapshot.Arc)
	}
	return fmt.Sprintf("%s | %s | %s | %s", snapshot.SnapshotID, snapshot.Kind, boundary, snapshot.CreatedAt)
}

func (m Model) handleRestoreCommand(args []string) (tea.Model, tea.Cmd) {
	if len(args) != 1 {
		return restoreCommandError(m, "Usage: /restore <snapshot-id> | confirm | cancel")
	}
	switch strings.ToLower(args[0]) {
	case "cancel":
		if m.restoreInFlight {
			return restoreCommandError(m, "A restore is already in progress; cannot cancel.")
		}
		if m.restoreStaged == "" {
			return restoreCommandError(m, "No restore confirmation is staged.")
		}
		m.restoreStaged = ""
		m.applyEvent(host.Event{Time: time.Now(), Category: "SYSTEM", Summary: "Restore confirmation cancelled.", Level: "info"})
		m.refreshEventViewport()
		return m, nil
	case "confirm":
		if m.restoreInFlight {
			return restoreCommandError(m, "A restore is already in progress; wait for it to complete.")
		}
		if m.restoreStaged == "" {
			return restoreCommandError(m, "No restore confirmation is staged. Use /restore <snapshot-id> first.")
		}
		if !m.runtime.IsEngineQuiescent() {
			return restoreCommandError(m, "Restore requires the engine to be stopped.")
		}
		snapshotID := m.restoreStaged
		m.restoreStaged = ""
		m.restoreInFlight = true
		m.applyEvent(host.Event{Time: time.Now(), Category: "SYSTEM", Summary: "Restoring snapshot " + snapshotID + "...", Level: "warn"})
		m.refreshEventViewport()
		return m, restoreSnapshot(m.runtime, snapshotID)
	default:
		if m.restoreInFlight {
			return restoreCommandError(m, "A restore is already in progress; cannot stage another.")
		}
		m.restoreStaged = args[0]
		m.applyEvent(host.Event{Time: time.Now(), Category: "SYSTEM", Summary: "Restore staged for " + args[0] + ". Listed files will overwrite existing files; extra files remain. A permanent rescue is created first. Run /restore confirm to proceed or /restore cancel to clear this confirmation.", Level: "warn"})
		m.refreshEventViewport()
		return m, nil
	}
}

func (m *Model) applyRestoreResult(msg restoreResultMsg) {
	m.restoreInFlight = false
	if msg.result != nil {
		var summary string
		if msg.result.RescuePath == "" {
			summary = fmt.Sprintf("Restore %s: no rescue created; attempted %d, succeeded %d, failed %d.", msg.result.SnapshotID, msg.result.Attempted, msg.result.Succeeded, msg.result.Failed)
		} else {
			summary = fmt.Sprintf("Restore %s: rescue %s; attempted %d, succeeded %d, failed %d.", msg.result.SnapshotID, msg.result.RescuePath, msg.result.Attempted, msg.result.Succeeded, msg.result.Failed)
		}
		m.applyEvent(host.Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: restoreResultLevel(msg)})
		for _, fileErr := range msg.result.FileErrors {
			m.applyEvent(host.Event{Time: time.Now(), Category: "ERROR", Summary: fmt.Sprintf("Restore file error: %s: %s", fileErr.Path, fileErr.Error), Level: "error"})
		}
	}
	if msg.err != nil {
		prefix := "Restore failed"
		if msg.result != nil {
			if msg.result.RescuePath == "" && msg.result.Succeeded == 0 {
				prefix = "Restore failed before applying changes"
			} else {
				prefix = "Restore applied; finalization paused"
			}
		}
		m.applyEvent(host.Event{Time: time.Now(), Category: "ERROR", Summary: prefix + ": " + msg.err.Error(), Level: "error"})
	}
}

func restoreResultLevel(msg restoreResultMsg) string {
	if msg.err != nil || msg.result.Failed > 0 {
		return "warn"
	}
	return "success"
}
