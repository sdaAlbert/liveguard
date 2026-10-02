//go:build liveapi

package monitor

import (
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMiniMaxLiveImageToolLoop(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("MINIMAX_API_KEY"))
	if key == "" {
		t.Fatal("MINIMAX_API_KEY is required for the live API smoke test")
	}
	imagePath := filepath.Join(t.TempDir(), "red-product.png")
	if err := writeRedProductImage(imagePath); err != nil {
		t.Fatal(err)
	}
	endpoint := strings.TrimSpace(os.Getenv("LIVEGUARD_MONITOR_ENDPOINT"))
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	reviewer := MiniMaxUnexpectedEventReviewer{APIKey: key, Model: "MiniMax-M3", Endpoint: endpoint}
	decision, err := reviewer.Review(ctx, UnexpectedEventInput{
		Goal:           "主播展示红色包装饮料时提醒我。字幕只能确认主播正在展示饮料，无法确认包装颜色，请查看当前画面判断是否触发。",
		TriggerFrame:   imagePath,
		RecentEvidence: []SignalEvent{{RoomID: "live-smoke", Source: "ocr", Text: "主播正在展示商品，请看商品包装颜色。", Confidence: .9}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tools := make([]string, 0, len(decision.ToolCalls))
	for _, call := range decision.ToolCalls {
		tools = append(tools, call.Tool)
	}
	t.Logf("MiniMax-M3 live response: action=%s category=%s tools=%v tokens=%d/%d latency_ms=%d", decision.Action, decision.Category, tools, decision.InputTokens, decision.OutputTokens, decision.LatencyMS)
	if !hasReviewTool(decision.ToolCalls, "inspect_live_frame") {
		t.Fatalf("M3 did not use the image tool for the user's visually dependent goal; tools=%v", tools)
	}
	if decision.VisualFinding == "" && decision.Action == "alert" {
		t.Fatal("an image-backed alert must preserve the visual finding")
	}
}

func TestMiniMaxLiveGoalPlanning(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("MINIMAX_API_KEY"))
	if key == "" {
		t.Fatal("MINIMAX_API_KEY is required for the live API smoke test")
	}
	planner := MiniMaxGoalPlanner{APIKey: key, Model: "MiniMax-M3", Endpoint: strings.TrimSpace(os.Getenv("LIVEGUARD_MONITOR_ENDPOINT"))}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	goal := "主播每说出一条新消息就尽量实时提醒我，不要只识别画面字幕。"
	plan, err := planner.Plan(ctx, goal)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MiniMax-M3 live monitoring plan: summary=%q capture_seconds=%d review_seconds=%d asr=%t vision=%s tools=%v", plan.Summary, plan.CaptureIntervalSeconds, plan.ReviewIntervalSeconds, plan.RecommendASR, plan.VisionPolicy, plan.Tools)
	if plan.Planner != "MiniMax-M3" || !plan.RecommendASR || !containsString(plan.Tools, "read_transcript_window") || plan.VisionPolicy == "goal_required" {
		t.Fatalf("M3 did not plan spoken-content monitoring from the user's goal: %#v", plan)
	}
}

func TestMiniMaxLiveASR(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("MINIMAX_API_KEY"))
	path := strings.TrimSpace(os.Getenv("LIVEGUARD_TEST_AUDIO"))
	if key == "" || path == "" {
		t.Fatal("MINIMAX_API_KEY and LIVEGUARD_TEST_AUDIO are required for the live ASR smoke test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	transcriber := MiniMaxTranscriber{APIKey: key, Language: "zh"}
	event, err := transcriber.Transcribe(ctx, AudioChunk{RoomID: "live-smoke", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MiniMax ASR live transcript: %q", event.Text)
	if event.Source != "asr" || strings.TrimSpace(event.Text) == "" {
		t.Fatalf("expected a non-empty ASR SignalEvent, got %#v", event)
	}
}

func writeRedProductImage(path string) error {
	canvas := image.NewRGBA(image.Rect(0, 0, 640, 480))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.RGBA{R: 235, G: 237, B: 233, A: 255}}, image.Point{}, draw.Src)
	// A simple red bottle silhouette makes the live vision question unambiguous.
	draw.Draw(canvas, image.Rect(264, 110, 376, 390), &image.Uniform{C: color.RGBA{R: 215, G: 36, B: 44, A: 255}}, image.Point{}, draw.Src)
	draw.Draw(canvas, image.Rect(288, 72, 352, 120), &image.Uniform{C: color.RGBA{R: 215, G: 36, B: 44, A: 255}}, image.Point{}, draw.Src)
	draw.Draw(canvas, image.Rect(289, 66, 351, 83), &image.Uniform{C: color.RGBA{R: 155, G: 24, B: 31, A: 255}}, image.Point{}, draw.Src)
	draw.Draw(canvas, image.Rect(273, 230, 367, 298), &image.Uniform{C: color.RGBA{R: 255, G: 255, B: 255, A: 255}}, image.Point{}, draw.Src)
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(file, canvas); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
