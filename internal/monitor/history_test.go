package monitor

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRepositoryBoundsMonitorHistory(t *testing.T) {
	repo := NewMemoryRepository()
	run := &Run{ID: "history", Status: RunRunning, StartedAt: time.Now(), Events: []SignalEvent{}, Investigations: []Investigation{}, Alerts: []Alert{}}
	if err := repo.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	_, err := repo.Update(context.Background(), run.ID, func(run *Run) error {
		for i := 0; i < maxRunEvents+1; i++ {
			run.Events = append(run.Events, SignalEvent{ID: fmt.Sprintf("event-%d", i)})
		}
		for i := 0; i < maxRunInvestigations+1; i++ {
			run.Investigations = append(run.Investigations, Investigation{ID: fmt.Sprintf("investigation-%d", i), Status: InvestigationWaiting, Evidence: make([]Evidence, maxInvestigationEvidence+1), Steps: make([]AgentStep, maxInvestigationSteps+1)})
		}
		for i := 0; i < maxRunAlerts+1; i++ {
			run.Alerts = append(run.Alerts, Alert{ID: fmt.Sprintf("alert-%d", i), Evidence: make([]Evidence, maxAlertEvidence+1)})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Events) > maxRunEvents || len(run.Investigations) > maxRunInvestigations || len(run.Alerts) > maxRunAlerts {
		t.Fatal("returned run exceeds configured history caps")
	}
	stored, err := repo.Get(context.Background(), "history")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.HistoryTruncated || len(stored.Events) != maxRunEvents || len(stored.Investigations) != maxRunInvestigations || len(stored.Alerts) != maxRunAlerts {
		t.Fatalf("history was not bounded and marked: events=%d investigations=%d alerts=%d truncated=%v", len(stored.Events), len(stored.Investigations), len(stored.Alerts), stored.HistoryTruncated)
	}
	latest := stored.Investigations[len(stored.Investigations)-1]
	if len(latest.Evidence) != maxInvestigationEvidence || len(latest.Steps) != maxInvestigationSteps || len(stored.Alerts[len(stored.Alerts)-1].Evidence) != maxAlertEvidence {
		t.Fatal("nested investigation and alert history was not capped")
	}
}
