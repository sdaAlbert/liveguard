package monitor

import (
	"context"
	"testing"
)

func TestStaticTextRecognizerProducesOCRSignal(t *testing.T) {
	recognizer := StaticTextRecognizer{Text: "新品今晚八点正式发布", Confidence: .91}
	event, err := recognizer.Recognize(context.Background(), VideoFrame{RoomID: "room-1", Path: "frame.png"})
	if err != nil {
		t.Fatal(err)
	}
	if event.Source != "ocr" || event.Text == "" || event.Confidence != .91 || event.RoomID != "room-1" {
		t.Fatalf("unexpected OCR signal: %#v", event)
	}
}

func TestTesseractRecognizerRequiresFramePath(t *testing.T) {
	_, err := (TesseractRecognizer{}).Recognize(context.Background(), VideoFrame{})
	if err == nil {
		t.Fatal("expected missing frame path error")
	}
}

func TestTesseractOCRCompactsSpacesBetweenChineseCharacters(t *testing.T) {
	text, confidence, err := parseTesseractTSV([]byte("level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n5\t1\t1\t1\t1\t1\t0\t0\t1\t1\t92\t抽\n5\t1\t1\t1\t1\t2\t0\t0\t1\t1\t90\t奖\n5\t1\t1\t1\t1\t3\t0\t0\t1\t1\t85\tcountdown\n"))
	if err != nil {
		t.Fatal(err)
	}
	if text != "抽奖 countdown" || confidence < .88 {
		t.Fatalf("unexpected normalized OCR signal: text=%q confidence=%f", text, confidence)
	}
}

func TestTesseractOCRPreservesLineBoundaries(t *testing.T) {
	text, _, err := parseTesseractTSV([]byte("level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n5\t1\t1\t1\t1\t1\t0\t0\t1\t1\t90\t平台提示\n5\t1\t1\t1\t2\t1\t0\t0\t1\t1\t88\t抽奖开始\n"))
	if err != nil {
		t.Fatal(err)
	}
	if text != "平台提示\n抽奖开始" {
		t.Fatalf("OCR line boundaries lost: %q", text)
	}
}
