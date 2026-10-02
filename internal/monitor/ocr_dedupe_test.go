package monitor

import (
	"testing"
	"time"
)

func TestOCRLineDeduperDropsRepeatedLinesButKeepsNewEvidence(t *testing.T) {
	var deduper OCRLineDeduper
	base := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)
	first := deduper.Filter("平台 合规提示！\n主播：欢迎大家", base)
	if first != "平台 合规提示！\n主播：欢迎大家" {
		t.Fatalf("first observation should be preserved, got %q", first)
	}
	repeated := deduper.Filter("平台合规提示\n主播：欢迎大家\n主播：抽奖马上开始", base.Add(6*time.Second))
	if repeated != "主播：抽奖马上开始" {
		t.Fatalf("repeated lines should be removed while novel text survives, got %q", repeated)
	}
	if negation := deduper.Filter("主播：不要抽奖", base.Add(12*time.Second)); negation != "主播：不要抽奖" {
		t.Fatalf("meaningful text changes must not be fuzzy-deduped, got %q", negation)
	}
}

func TestOCRLineDeduperExpiresFingerprints(t *testing.T) {
	var deduper OCRLineDeduper
	base := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)
	deduper.Filter("平台固定提示", base)
	if got := deduper.Filter("平台固定提示", base.Add(ocrLineDedupeWindow)); got != "平台固定提示" {
		t.Fatalf("expired text should be allowed again, got %q", got)
	}
}
