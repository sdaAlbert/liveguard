package monitor

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
	"unicode"
)

const alertDedupeWindow = 2 * time.Minute

func normalizedEventText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, strings.TrimSpace(text))
}

// Same category is history to inspect, not proof of the same event. This keeps
// subsequent announcements/news usable without disabling duplicate prevention.
func hasRecentSemanticAlert(alerts []Alert, event SignalEvent, _ string, summary string) bool {
	now := event.ObservedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	textKey, summaryKey := normalizedEventText(event.Text), normalizedEventText(summary)
	for _, alert := range alerts {
		age := now.Sub(alert.CreatedAt)
		if alert.RoomID != event.RoomID || age < 0 || age > alertDedupeWindow {
			continue
		}
		if summaryKey != "" && summaryKey == normalizedEventText(alert.Summary) {
			return true
		}
		for _, evidence := range alert.Evidence {
			if textKey != "" && textKey == normalizedEventText(evidence.Text) {
				return true
			}
		}
	}
	return false
}

func semanticDedupeKey(roomID, eventType, summary string) string {
	key := sha256.Sum256([]byte(normalizedEventText(summary)))
	return fmt.Sprintf("%s:%s:%x", roomID, eventType, key[:8])
}
