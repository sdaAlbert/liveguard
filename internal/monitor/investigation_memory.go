package monitor

import (
	"strings"
	"time"
)

const (
	investigationMemoryTTL       = 24 * time.Hour
	maxInvestigationMemoryStored = 32
	maxInvestigationMemoryInput  = 5
	maxMemoryEvidence            = 3
)

// updateInvestigationMemory stores only completed outcomes from the current
// goal version. The source investigation and observation ledger remain the
// authoritative records.
func updateInvestigationMemory(run *Run, investigation Investigation, decision UnexpectedEventDecision, now time.Time) {
	if run == nil || investigation.ID == "" || investigation.GoalVersion != run.GoalVersion || decision.Failure != "" {
		return
	}
	if decision.StopReason != "finished" && decision.StopReason != "terminal_tool" {
		return
	}
	if decision.Action != "wait" && decision.Action != "ignore" && decision.Action != "alert" {
		return
	}
	if decision.Action == "alert" && investigation.Status != InvestigationConfirmed {
		return
	}
	note := strings.TrimSpace(investigation.Conclusion)
	if note == "" || len(investigation.Evidence) == 0 {
		return
	}
	evidence := make([]Evidence, 0, maxMemoryEvidence)
	latestEvidenceAt := time.Time{}
	for _, item := range investigation.Evidence {
		if item.EventID == "" {
			continue
		}
		if item.GoalVersion == 0 {
			item.GoalVersion = investigation.GoalVersion
		}
		if item.RoomID == "" {
			item.RoomID = investigation.RoomID
		}
		item.Text = boundedReviewText(item.Text, 280)
		item.TextRegions = nil
		evidence = append(evidence, item)
		if item.ObservedAt.After(latestEvidenceAt) {
			latestEvidenceAt = item.ObservedAt
		}
		if len(evidence) == maxMemoryEvidence {
			break
		}
	}
	if len(evidence) == 0 {
		return
	}
	if latestEvidenceAt.IsZero() {
		latestEvidenceAt = now
	}
	expiresAt := latestEvidenceAt.Add(investigationMemoryTTL)
	if !expiresAt.After(now) {
		return
	}
	memory := InvestigationMemory{
		GoalVersion: investigation.GoalVersion,
		ID:          investigation.ID,
		RoomID:      investigation.RoomID,
		EventType:   investigation.EventType,
		Outcome:     investigation.Status,
		Note:        boundedReviewText(note, 360),
		Evidence:    evidence,
		CreatedAt:   now.UTC(),
		ExpiresAt:   expiresAt.UTC(),
	}

	replaced := false
	for i := range run.InvestigationMemory {
		if run.InvestigationMemory[i].ID == memory.ID {
			run.InvestigationMemory[i] = memory
			replaced = true
			break
		}
	}
	if !replaced {
		run.InvestigationMemory = append(run.InvestigationMemory, memory)
	}
	pruneInvestigationMemory(run, now)
}

func pruneInvestigationMemory(run *Run, now time.Time) {
	if run == nil {
		return
	}
	active := run.InvestigationMemory[:0]
	for _, memory := range run.InvestigationMemory {
		if memory.GoalVersion != run.GoalVersion || memory.ExpiresAt.IsZero() || !memory.ExpiresAt.After(now) {
			continue
		}
		active = append(active, memory)
	}
	if len(active) > maxInvestigationMemoryStored {
		active = active[len(active)-maxInvestigationMemoryStored:]
	}
	run.InvestigationMemory = append([]InvestigationMemory(nil), active...)
}

func selectInvestigationMemory(memories []InvestigationMemory, goalVersion uint64, roomID string, now time.Time) []InvestigationMemory {
	active := make([]InvestigationMemory, 0, len(memories))
	for _, memory := range memories {
		if memory.GoalVersion != goalVersion || memory.ExpiresAt.IsZero() || !memory.ExpiresAt.After(now) || strings.TrimSpace(memory.Note) == "" {
			continue
		}
		memory.Note = boundedReviewText(memory.Note, 360)
		memory.Evidence = append([]Evidence(nil), memory.Evidence...)
		if len(memory.Evidence) > maxMemoryEvidence {
			memory.Evidence = memory.Evidence[:maxMemoryEvidence]
		}
		for i := range memory.Evidence {
			memory.Evidence[i].Text = boundedReviewText(memory.Evidence[i].Text, 280)
			memory.Evidence[i].TextRegions = nil
		}
		active = append(active, memory)
	}
	// Prefer same-room continuity, then newer notes. Room-agnostic history is
	// still available when the current event belongs to another room.
	for i := 0; i < len(active); i++ {
		for j := i + 1; j < len(active); j++ {
			iRoom, jRoom := active[i].RoomID == roomID, active[j].RoomID == roomID
			if (!iRoom && jRoom) || (iRoom == jRoom && active[j].CreatedAt.After(active[i].CreatedAt)) {
				active[i], active[j] = active[j], active[i]
			}
		}
	}
	if len(active) > maxInvestigationMemoryInput {
		active = active[:maxInvestigationMemoryInput]
	}
	return active
}
