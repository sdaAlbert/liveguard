package monitor

import (
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"liveguard/internal/agent"
)

func TestAgentOnlyArchivesFrameWhenSaveToolIsCalled(t *testing.T) {
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
	ocr := &SignalEvent{ID: "frame-1:ocr", Source: SignalOCR, Text: "限时活动", ObservedAt: time.Now().UTC()}
	savedCalls := 0
	input := UnexpectedEventInput{
		RecentEvidence: []SignalEvent{{ID: "trigger-1", RoomID: "room-1", Source: SignalASR, Text: "主播开始介绍活动"}},
		CaptureFrame: func(context.Context) (FrameAsset, error) {
			return FrameAsset{Path: framePath, ObservationID: "frame-1", ObservedAt: time.Now().UTC(), OCR: ocr}, nil
		},
		SaveFrame: func(_ context.Context, asset FrameAsset) (FrameAsset, error) {
			savedCalls++
			asset.ArchiveID = "archive-id"
			return asset, nil
		},
	}
	reviewer := &AgentReviewer{}
	state := reviewState{readEvidenceTurn: -1, searchAlertsTurn: -1, frameInspectTurn: -1}
	inspect := agent.ToolCall{Name: "inspect_live_frame", Arguments: json.RawMessage(`{}`)}
	result, _, terminal := reviewer.executeTool(context.Background(), inspect, input, nil, &state, 1)
	if result.IsError || terminal || !state.frameInspected || savedCalls != 0 {
		t.Fatalf("inspection should return the frame without archiving it: result=%#v state=%#v saved=%d", result, state, savedCalls)
	}
	if _, err := os.Stat(framePath); err != nil {
		t.Fatalf("temporary image must remain available until the review ends: %v", err)
	}
	if state.frameArchiveID != "" {
		t.Fatalf("inspection unexpectedly assigned an archive id: %q", state.frameArchiveID)
	}

	save := agent.ToolCall{Name: "save_frame_evidence", Arguments: json.RawMessage(`{}`)}
	result, _, terminal = reviewer.executeTool(context.Background(), save, input, nil, &state, 2)
	if result.IsError || terminal || savedCalls != 1 || state.frameArchiveID != "archive-id" {
		t.Fatalf("explicit save should archive the inspected frame: result=%#v state=%#v saved=%d", result, state, savedCalls)
	}
	if len(state.historyEvidence) != 1 || state.historyEvidence[0].FrameArchiveID != "archive-id" {
		t.Fatalf("the OCR evidence from the saved image must link to that image: %#v", state.historyEvidence)
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
	source := filepath.Join(t.TempDir(), "source.png")
	file, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	archiveID, err := service.archiveFrame(run.ID, "image-event", source)
	if err != nil {
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
