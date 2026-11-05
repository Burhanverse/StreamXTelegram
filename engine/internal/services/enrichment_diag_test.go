package services

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnrichmentDecisionLogFile(t *testing.T) {
	tmpDir := t.TempDir()
	s := &EnrichmentService{}
	s.SetDebug(true, tmpDir)

	record := EnrichmentRecord{
		Timestamp: "2026-11-05T12:00:00Z",
		TrackID:   "track_hindi_123",
		RawTags: map[string]string{
			"title":  "Tum Hi Ho (From Aashiqui 2)",
			"artist": "Arijit Singh",
			"album":  "Aashiqui 2",
		},
		RawFilename: "01 Tum Hi Ho [SongsPK.info] 320Kbps.mp3",
		Cleaned: map[string]string{
			"title":  "Tum Hi Ho",
			"artist": "Arijit Singh",
		},
		QueriesSent: []string{
			"Tum Hi Ho Arijit Singh",
			"Arijit Singh Tum Hi Ho",
		},
		Providers: map[string]ProviderLog{
			"jiosaavn": {
				Queries: []string{"Tum Hi Ho Arijit Singh"},
				Candidates: []CandidateItem{
					{
						Title:       "Tum Hi Ho",
						Artist:      "Arijit Singh",
						DurationSec: 262,
					},
					{
						Title:       "Tum Hi Ho Unplugged",
						Artist:      "Arijit Singh",
						DurationSec: 275,
					},
				},
			},
		},
		Decision: DecisionLogDetails{
			Provider: "jiosaavn",
			Rule:     "exact",
			AcceptedCandidate: &CandidateItem{
				Title:       "Tum Hi Ho",
				Artist:      "Arijit Singh",
				DurationSec: 262,
			},
		},
		FinalCoverURL: "https://c.saavncdn.com/123/Tum-Hi-Ho-Hindi-2013-500x500.jpg",
	}

	s.logEnrichmentRecord(record)

	// Verify RingBuffer
	recent := s.GetRecentEnrichmentLogs(10)
	if len(recent) != 1 {
		t.Fatalf("expected 1 recent log in ring buffer, got %d", len(recent))
	}

	// Verify enrich.jsonl file
	logFile := filepath.Join(tmpDir, "logs", "enrich.jsonl")
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read enrich.jsonl: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line in enrich.jsonl, got %d", len(lines))
	}

	var parsed EnrichmentRecord
	if err := json.Unmarshal([]byte(lines[0]), &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON line: %v", err)
	}

	if parsed.TrackID != "track_hindi_123" {
		t.Fatalf("unexpected trackID: %s", parsed.TrackID)
	}
	if parsed.Decision.Provider != "jiosaavn" || parsed.Decision.Rule != "exact" {
		t.Fatalf("unexpected decision: %+v", parsed.Decision)
	}
	if parsed.Decision.AcceptedCandidate == nil || parsed.Decision.AcceptedCandidate.Title != "Tum Hi Ho" {
		t.Fatalf("unexpected accepted candidate: %+v", parsed.Decision.AcceptedCandidate)
	}
	if parsed.FinalCoverURL != record.FinalCoverURL {
		t.Fatalf("unexpected cover URL: %s", parsed.FinalCoverURL)
	}
	if len(parsed.Providers["jiosaavn"].Candidates) != 2 {
		t.Fatalf("expected 2 candidates in provider log, got %d", len(parsed.Providers["jiosaavn"].Candidates))
	}
}

func TestEnrichmentLogFileRotation(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "enrich.jsonl")
	rf := newEnrichmentRotatedFile(logPath, 80) // 80 bytes max

	err := rf.Append([]byte(`{"track":"1"}`))
	if err != nil {
		t.Fatal(err)
	}

	// Now append something that pushes total over 80 bytes
	err = rf.Append([]byte(strings.Repeat("X", 85)))
	if err != nil {
		t.Fatal(err)
	}

	backupPath := logPath + ".1"
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("expected backup %s to exist: %v", backupPath, err)
	}
}

