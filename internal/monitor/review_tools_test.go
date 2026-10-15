package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"liveguard/internal/agent"
)

func TestAgentOnlyKeepsSelectedFrameTemporary(t *testing.T) {
	framePath := filepath.Join(t.TempDir(), "temporary-frame.png")
	file, err := os.Create(framePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	ocr := &SignalEvent{ID: "frame-1:ocr", ParentEventID: "frame-1", Source: SignalOCR, Text: "限时活动", ObservedAt: time.Now().UTC()}
	savedCalls := 0
	recordedCalls := 0
	input := UnexpectedEventInput{
		RecentEvidence: []SignalEvent{{ID: "trigger-1", RoomID: "room-1", Source: SignalASR, Text: "主播开始介绍活动"}},
		CaptureFrame: func(context.Context) (FrameAsset, error) {
			return FrameAsset{Path: framePath, ObservationID: "frame-1", ObservedAt: time.Now().UTC(), OCR: ocr}, nil
		},
		RecordFrameEvidence: func(_ context.Context, asset FrameAsset) error {
			recordedCalls++
			if asset.OCR == nil || asset.Path != framePath {
				t.Fatalf("selected frame should record only associated text evidence: %#v", asset)
			}
			return nil
		},
	}
	reviewer := &AgentReviewer{}
	state := reviewState{readEvidenceTurn: -1, searchAlertsTurn: -1, frameInspectTurn: -1}
	inspect := agent.ToolCall{Name: "inspect_live_frame", Arguments: json.RawMessage(`{}`)}
	result, _, terminal := reviewer.executeTool(context.Background(), inspect, input, nil, &state, 1)
	if result.IsError || terminal || !state.frameInspected || savedCalls != 0 || recordedCalls != 0 {
		t.Fatalf("inspection should return the frame without persisting it: result=%#v state=%#v", result, state)
	}
	if _, err := os.Stat(framePath); err != nil {
		t.Fatalf("temporary image must remain available until the review ends: %v", err)
	}
	if state.frameArchiveID != "" {
		t.Fatalf("inspection unexpectedly assigned an archive id: %q", state.frameArchiveID)
	}

	save := agent.ToolCall{Name: "save_frame_evidence", Arguments: json.RawMessage(`{}`)}
	result, _, terminal = reviewer.executeTool(context.Background(), save, input, nil, &state, 2)
	if result.IsError || terminal || recordedCalls != 1 || state.frameArchiveID != "" || !state.frameSavedForReview {
		t.Fatalf("explicit selection should keep the inspected frame temporary: result=%#v state=%#v recorded=%d", result, state, recordedCalls)
	}
	if len(state.historyEvidence) != 1 || state.historyEvidence[0].FrameArchiveID != "" {
		t.Fatalf("the OCR evidence should be retained without a persisted image link: %#v", state.historyEvidence)
	}
	if _, err := os.Stat(framePath); err != nil {
		t.Fatalf("temporary image should remain available until the investigation ends: %v", err)
	}
}

func TestArchivedFramePathRequiresRunEvidenceReference(t *testing.T) {
	service := NewService(NewMemoryRepository(), nil)
	service.SetFrameArchiveDir(t.TempDir())
	defer service.StopAll()
	run, err := service.Start(context.Background(), StartInput{TargetURL: "https://example.com/live", Goal: "关注公告", SourceMode: "ocr_upload"})
	if err != nil {
		t.Fatal(err)
	}
	archiveID := strings.Repeat("a", 64)
	runHash := sha256.Sum256([]byte(run.ID))
	directory := filepath.Join(service.frameArchiveDir, hex.EncodeToString(runHash[:]))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	pathFixture := filepath.Join(directory, archiveID+".png")
	if err := os.WriteFile(pathFixture, []byte("legacy screenshot fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ArchivedFramePath(context.Background(), run.ID, archiveID); err == nil {
		t.Fatal("an unreferenced image must not be exposed by the HTTP reader")
	}
	_, err = service.repository.Update(context.Background(), run.ID, func(run *Run) error {
		run.Investigations = append(run.Investigations, Investigation{ID: "investigation-1", Status: InvestigationSuppressed, Evidence: []Evidence{{FrameArchiveID: archiveID}}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	path, err := service.ArchivedFramePath(context.Background(), run.ID, archiveID)
	if err != nil {
		t.Fatalf("referenced image should be available: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("archive path does not exist: %v", err)
	}
}
