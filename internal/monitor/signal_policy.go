package monitor

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var countdownClock = regexp.MustCompile(`(?:倒计时|剩余|还有)\s*(\d{1,2}):(\d{2})`)
var relativeEventTime = regexp.MustCompile(`(\d{1,3})\s*(秒|分钟|小时)\s*后`)
var numericValue = regexp.MustCompile(`\d+(?:\.\d+)?`)

// Only otherwise identical statements with changed numeric tokens qualify.
// Both observations remain in the ledger, while pending work uses the update.
func numericRevision(a, b SignalEvent) bool {
	if a.RoomID != b.RoomID || a.Source != b.Source || a.Source == SignalFrame || b.ObservedAt.Before(a.ObservedAt) {
		return false
	}
	left, right := numericValue.FindAllString(a.Text, -1), numericValue.FindAllString(b.Text, -1)
	if len(left) == 0 || len(left) != len(right) || strings.Join(left, "|") == strings.Join(right, "|") {
		return false
	}
	return normalizeOCRLine(numericValue.ReplaceAllString(a.Text, "#")) == normalizeOCRLine(numericValue.ReplaceAllString(b.Text, "#"))
}

// These are execution deadlines, not Run lifetimes or a promise to wake at a
// future time. An explicit countdown overrides the generic live-text window.
func signalDeadline(event SignalEvent) time.Time {
	if !event.ValidUntil.IsZero() {
		return event.ValidUntil
	}
	horizon := pendingSignalTTL
	if event.Source == SignalFrame {
		horizon = 30 * time.Second
	}
	if match := countdownClock.FindStringSubmatch(event.Text); len(match) > 0 {
		minutes, _ := strconv.Atoi(match[1])
		seconds, _ := strconv.Atoi(match[2])
		if seconds < 60 {
			horizon = time.Duration(minutes*60+seconds) * time.Second
		}
	} else if match := relativeEventTime.FindStringSubmatch(event.Text); len(match) > 0 {
		n, _ := strconv.Atoi(match[1])
		unit := time.Second
		if match[2] == "分钟" {
			unit = time.Minute
		} else if match[2] == "小时" {
			unit = time.Hour
		}
		if n > 0 {
			horizon = time.Duration(n) * unit
		}
	}
	return event.ObservedAt.Add(horizon)
}

func pendingPriority(event SignalEvent) int {
	if event.Source == SignalFrame {
		return -1
	}
	if containsAny(event.Text, []string{"取消", "作废", "更正", "终止", "延期", "不再", "改为", "停止"}) {
		return 2
	}
	return 0
}

func samePendingObservation(a, b SignalEvent) bool {
	return a.Source != SignalFrame && a.Source == b.Source && a.RoomID == b.RoomID && strings.TrimSpace(a.Text) == strings.TrimSpace(b.Text) && b.ObservedAt.Sub(a.ObservedAt) >= 0 && b.ObservedAt.Sub(a.ObservedAt) <= 30*time.Second
}
