package monitor

import (
	"testing"
	"time"
)

func TestOCRPresenceTrackerEmitsAppearanceAfterBaseline(t *testing.T) {
	var tracker OCRPresenceTracker
	tracker.Configure(1, []string{"iPhone 17"})
	start := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)

	transitions, commit := tracker.Preview("《主屏幕》直播开始", start)
	if len(transitions) != 0 {
		t.Fatalf("baseline sample must not emit a transition: %#v", transitions)
	}
	commit()

	transitions, commit = tracker.Preview("《主屏幕》iPhone 17 现在有货", start.Add(time.Second))
	if len(transitions) != 1 || transitions[0].Kind != "appeared" || transitions[0].Term != "iPhone 17" || transitions[0].MatchedText != "《主屏幕》iPhone 17 现在有货" {
		t.Fatalf("expected an appearance transition with the matched OCR line: %#v", transitions)
	}
	commit()

	transitions, _ = tracker.Preview("iPhone 17 现在有货", start.Add(2*time.Second))
	if len(transitions) != 0 {
		t.Fatalf("continued presence must not emit repeated appearance signals: %#v", transitions)
	}
}

func TestOCRPresenceTrackerRequiresThreeSuccessfulMissesAndSupportsReappearance(t *testing.T) {
	var tracker OCRPresenceTracker
	tracker.Configure(1, []string{"目标商品"})
	start := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	_, commit := tracker.Preview("目标商品 价格 99 元", start)
	commit()

	for i := 1; i < presenceMissingSamplesRequired; i++ {
		transitions, commit := tracker.Preview("其他直播文字", start.Add(time.Duration(i)*time.Second))
		if len(transitions) != 0 {
			t.Fatalf("miss %d must not emit disappearance before the threshold: %#v", i, transitions)
		}
		commit()
	}

	transitions, commit := tracker.Preview("其他直播文字", start.Add(3*time.Second))
	if len(transitions) != 1 || transitions[0].Kind != "disappeared" || transitions[0].MatchedText != "目标商品 价格 99 元" || !transitions[0].LastSeenAt.Equal(start) {
		t.Fatalf("third successful miss must emit disappearance with last-seen evidence: %#v", transitions)
	}
	commit()

	transitions, _ = tracker.Preview("目标商品 补货", start.Add(4*time.Second))
	if len(transitions) != 1 || transitions[0].Kind != "appeared" {
		t.Fatalf("target reappearance after disappearance must emit a new transition: %#v", transitions)
	}
}

func TestOCRPresenceTrackerPreviewCommitsOnlyAfterAdmission(t *testing.T) {
	var tracker OCRPresenceTracker
	tracker.Configure(1, []string{"商品A"})
	_, commit := tracker.Preview("普通画面", time.Now().UTC())
	commit()

	appearance := "商品A 上架"
	first, _ := tracker.Preview(appearance, time.Now().UTC())
	second, _ := tracker.Preview(appearance, time.Now().UTC())
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("uncommitted preview must remain retryable after rejected admission: first=%#v second=%#v", first, second)
	}

	_, commit = tracker.Preview(appearance, time.Now().UTC())
	commit()
	transitions, _ := tracker.Preview(appearance, time.Now().UTC())
	if len(transitions) != 0 {
		t.Fatalf("committed appearance must not be emitted again while still present: %#v", transitions)
	}
}

func TestPresenceTermsRequireExplicitAbsenceGoalAndGoalTextMatch(t *testing.T) {
	if terms := presenceTermsFromGoal("每条新消息出现时提醒我"); len(terms) != 0 {
		t.Fatalf("generic new-message appearance must not enable target tracking: %#v", terms)
	}

	terms := presenceTermsFromGoal("iPhone 17 出现和消失时提醒我")
	if len(terms) != 1 || normalizeOCRLine(terms[0]) != "iphone17" {
		t.Fatalf("expected the named target extracted from the goal, got %#v", terms)
	}

	validated := normalizePresenceTerms([]string{"iPhone 17", "unmentioned product", "商品"}, "iPhone 17 上架后下架提醒我")
	if len(validated) != 1 || validated[0] != "iPhone 17" {
		t.Fatalf("only specific terms present in the goal should be tracked: %#v", validated)
	}
}
