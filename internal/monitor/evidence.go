package monitor

import (
	"errors"
	"time"
)

const liveEvidenceAge = 2 * time.Minute

// Citations resolve only against the immutable trigger and the pages actually
// returned in this investigation. History can explain the past; live alerts
// must cite fresh evidence from the current goal.
func resolveReviewCitations(input UnexpectedEventInput, state *reviewState, ids []string, basis string, now time.Time) ([]Evidence, error) {
	if len(ids) == 0 || len(ids) > 6 || (basis != "live" && basis != "history") {
		return nil, errors.New("提醒需要明确的证据引用和 live/history 口径")
	}
	if basis == "history" && !state.searchedHistory {
		return nil, errors.New("历史结论必须先明确查询整场观测")
	}
	catalog := map[string]SignalEvent{}
	catalog[input.RecentEvidence[0].ID] = input.RecentEvidence[0]
	for _, event := range state.historyEvidence {
		catalog[event.ID] = event
	}
	seen := map[string]bool{}
	var result []Evidence
	for _, id := range ids {
		event, ok := catalog[id]
		if !ok || seen[id] || id == "" || event.RoomID != input.RecentEvidence[0].RoomID {
			return nil, errors.New("证据引用未知、重复或来自其他房间")
		}
		seen[id] = true
		if event.Source != SignalOCR && event.Source != SignalASR && event.Source != SignalVision && !(event.Source == SignalFrame && state.frameInspected && state.frameEventID == id) {
			return nil, errors.New("截图采样标记必须实际看图后才能引用")
		}
		if basis == "live" && !currentEvidence(event.ObservedAt, event.ValidUntil, event.GoalVersion, input.GoalVersion, input.GoalUpdatedAt, now) {
			return nil, errors.New("即时提醒引用了过期或旧目标证据；历史结论请使用 history")
		}
		result = append(result, evidenceFromSignal(event))
	}
	if state.frameInspected && basis == "live" && !currentEvidence(state.frameObservedAt, state.frameObservedAt.Add(30*time.Second), input.GoalVersion, input.GoalVersion, input.GoalUpdatedAt, now) {
		return nil, errors.New("看图证据已经过期，不能用于即时提醒")
	}
	return result, nil
}

func currentEvidence(at, validUntil time.Time, version, goalVersion uint64, goalUpdatedAt, now time.Time) bool {
	if validUntil.IsZero() {
		validUntil = at.Add(liveEvidenceAge)
	}
	return !at.IsZero() && now.Before(validUntil) && !at.After(now.Add(maxFutureSignalSkew)) && (goalUpdatedAt.IsZero() || !at.Before(goalUpdatedAt)) && (version == 0 || version == goalVersion)
}

func evidenceFromSignal(event SignalEvent) Evidence {
	return Evidence{EventID: event.ID, RoomID: event.RoomID, GoalVersion: event.GoalVersion, Source: string(event.Source), Text: boundedReviewText(event.Text, 400), Confidence: event.Confidence, ObservedAt: event.ObservedAt, ValidUntil: event.ValidUntil, FrameArchiveID: event.FrameArchiveID, TextRegions: boundedTextRegions(event.TextRegions)}
}

func validCommittedCitations(run *Run, event SignalEvent, d UnexpectedEventDecision, now time.Time) bool {
	if len(d.UsedEvidence) == 0 || len(d.UsedEvidence) > 6 || (d.EvidenceBasis != "live" && d.EvidenceBasis != "history") {
		return false
	}
	for _, cited := range d.UsedEvidence {
		if cited.RoomID != event.RoomID {
			return false
		}
		if d.EvidenceBasis == "live" && !currentEvidence(cited.ObservedAt, cited.ValidUntil, cited.GoalVersion, run.GoalVersion, run.GoalUpdatedAt, now) {
			return false
		}
	}
	if d.EvidenceBasis == "live" && hasReviewTool(d.ToolCalls, "inspect_live_frame") && !currentEvidence(d.VisualObservedAt, d.VisualObservedAt.Add(30*time.Second), run.GoalVersion, run.GoalVersion, run.GoalUpdatedAt, now) {
		return false
	}
	return true
}
