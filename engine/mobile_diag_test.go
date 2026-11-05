package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnosticsClipboardExport(t *testing.T) {
	tmpDir := t.TempDir()

	// Create filesDir/logs/stream_stats.log
	logDir := filepath.Join(tmpDir, "logs")
	_ = os.MkdirAll(logDir, 0755)

	statsFile := filepath.Join(logDir, "stream_stats.log")
	statsContent := "[STREAM-STATS] [ROLLING] track=1 reads=5 sizes=100/200/300\n[STREAM-STATS] [RELEASE] track=1 reads=10 sizes=100/200/300\n"
	_ = os.WriteFile(statsFile, []byte(statsContent), 0644)

	enrichFile := filepath.Join(logDir, "enrich.jsonl")
	enrichContent := `{"track_id":"1","decision":{"provider":"jiosaavn","rule":"exact"}}` + "\n"
	_ = os.WriteFile(enrichFile, []byte(enrichContent), 0644)

	globalMu.Lock()
	globalFilesDir = tmpDir
	globalMu.Unlock()

	defer func() {
		globalMu.Lock()
		globalFilesDir = ""
		globalMu.Unlock()
	}()

	statsOutput := GetStreamingStats(200)
	if !strings.Contains(statsOutput, "[STREAM-STATS] [RELEASE]") {
		t.Fatalf("unexpected streaming stats output: %s", statsOutput)
	}

	enrichOutput := GetEnrichmentLogs(50)
	if !strings.Contains(enrichOutput, `"track_id":"1"`) {
		t.Fatalf("unexpected enrichment logs output: %s", enrichOutput)
	}

	clipboardOutput := GetDiagnosticsClipboard(200, 50)
	if !strings.Contains(clipboardOutput, "=== STREAMING STATS (Last 2 lines) ===") {
		t.Fatalf("clipboard missing streaming stats header: %s", clipboardOutput)
	}
	if !strings.Contains(clipboardOutput, "=== ENRICHMENT DECISIONS (Last 1 lines) ===") {
		t.Fatalf("clipboard missing enrichment decisions header: %s", clipboardOutput)
	}
}

