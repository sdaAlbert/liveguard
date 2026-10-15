package monitor

import (
	"strings"
	"time"
	"unicode"
)

const (
	ocrLineDedupeWindow    = 30 * time.Minute
	maxOCRLineFingerprints = 512
)

// OCRLineDeduper drops repeated visible lines before they become Agent signals.
// It tolerates one OCR character flip only for long lines with unchanged
// numbers and event-state words. Call it once per monitor capture stream.
type OCRLineDeduper struct {
	seen map[string]time.Time
}

// Preview applies the usual exact dedupe rule to a private copy. The returned
// commit records observations only after downstream admission succeeds.
func (d *OCRLineDeduper) Preview(text string, now time.Time) (string, func()) {
	next := OCRLineDeduper{seen: make(map[string]time.Time, len(d.seen))}
	for key, at := range d.seen {
		next.seen[key] = at
	}
	filtered := next.Filter(text, now)
	return filtered, func() { d.seen = next.seen }
}

// Filter returns only lines not observed during the recent dedupe window.
func (d *OCRLineDeduper) Filter(text string, now time.Time) string {
	if d.seen == nil {
		d.seen = make(map[string]time.Time)
	}
	if now.IsZero() {
		now = time.Now()
	}
	for key, observedAt := range d.seen {
		if now.Sub(observedAt) >= ocrLineDedupeWindow || now.Before(observedAt) {
			delete(d.seen, key)
		}
	}

	var novel []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		key := normalizeOCRLine(line)
		if key == "" {
			continue
		}
		if _, exists := d.seen[key]; exists {
			continue
		}
		if repeatedOCRVariant(key, d.seen) {
			continue
		}
		d.seen[key] = now
		novel = append(novel, line)
	}
	for len(d.seen) > maxOCRLineFingerprints {
		var oldestKey string
		var oldest time.Time
		for key, at := range d.seen {
			if oldestKey == "" || at.Before(oldest) {
				oldestKey, oldest = key, at
			}
		}
		delete(d.seen, oldestKey)
	}
	return strings.Join(novel, "\n")
}

// repeatedOCRVariant tolerates one OCR character flip in a long resident line.
// Short lines, numbers, and event-state words remain exact because a tiny
// change there can carry real meaning (for example a changed price or negation).
func repeatedOCRVariant(candidate string, seen map[string]time.Time) bool {
	if runeCount(candidate) < 14 {
		return false
	}
	for existing := range seen {
		if runeCount(existing) < 14 || ocrSensitiveSignature(candidate) != ocrSensitiveSignature(existing) {
			continue
		}
		if withinOneRuneEdit(candidate, existing) {
			return true
		}
	}
	return false
}

func runeCount(value string) int { return len([]rune(value)) }

func withinOneRuneEdit(left, right string) bool {
	a, b := []rune(left), []rune(right)
	if len(a)-len(b) > 1 || len(b)-len(a) > 1 {
		return false
	}
	i, j, edits := 0, 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			i++
			j++
			continue
		}
		if edits == 1 {
			return false
		}
		edits++
		if len(a) > len(b) {
			i++
		} else if len(b) > len(a) {
			j++
		} else {
			i++
			j++
		}
	}
	if i < len(a) || j < len(b) {
		edits++
	}
	return edits <= 1
}

func ocrSensitiveSignature(text string) string {
	var signature []string
	for _, token := range []string{"不", "没", "未", "别", "取消", "作废", "更正", "延期", "不再", "停止", "暂停", "恢复", "开始", "开启", "开放", "结束", "截止", "关闭", "开奖", "抽奖", "报名", "上架", "下架", "补货", "售罄", "有货", "缺货", "限时", "秒杀", "优惠", "元"} {
		if strings.Contains(text, token) {
			signature = append(signature, "t:"+token)
		}
	}
	var digits strings.Builder
	for _, r := range text {
		if unicode.IsDigit(r) {
			digits.WriteRune(r)
		}
	}
	if digits.Len() > 0 {
		signature = append(signature, "n:"+digits.String())
	}
	return strings.Join(signature, "|")
}

func normalizeOCRLine(line string) string {
	line = strings.TrimPrefix(line, "《主屏幕》")
	line = strings.TrimPrefix(line, "《聊天区》")
	var normalized strings.Builder
	for _, r := range strings.ToLower(line) {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			continue
		}
		normalized.WriteRune(r)
	}
	return normalized.String()
}
