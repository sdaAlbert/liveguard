package monitor

import (
	"strings"
	"time"
	"unicode"
)

const (
	ocrLineDedupeWindow    = 2 * time.Minute
	maxOCRLineFingerprints = 512
)

// OCRLineDeduper drops repeated visible lines before they become Agent signals.
// It is intentionally exact after whitespace/punctuation normalization: fuzzy
// matching could erase a meaningful one-character change such as a negation or
// amount. Call it once per monitor capture stream.
type OCRLineDeduper struct {
	seen map[string]time.Time
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

func normalizeOCRLine(line string) string {
	var normalized strings.Builder
	for _, r := range strings.ToLower(line) {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			continue
		}
		normalized.WriteRune(r)
	}
	return normalized.String()
}
