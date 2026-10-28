package monitor

import "time"

const (
	maxRunEvents             = 2000
	maxRunInvestigations     = 500
	maxRunAlerts             = 500
	maxInvestigationEvidence = 100
	maxInvestigationSteps    = 64
	maxAlertEvidence         = 100
)

// boundRunHistory applies the same bounded-retention policy before either
// repository commits a snapshot. Newest records are kept for the control room.
func boundRunHistory(run *Run) {
	if run == nil {
		return
	}
	pruneInvestigationMemory(run, time.Now().UTC())
	if len(run.Events) > maxRunEvents {
		run.Events = append([]SignalEvent(nil), run.Events[len(run.Events)-maxRunEvents:]...)
		run.HistoryTruncated = true
	}
	if len(run.Investigations) > maxRunInvestigations {
		run.Investigations = append([]Investigation(nil), run.Investigations[len(run.Investigations)-maxRunInvestigations:]...)
		run.HistoryTruncated = true
	}
	for i := range run.Investigations {
		if len(run.Investigations[i].Evidence) > maxInvestigationEvidence {
			run.Investigations[i].Evidence = append([]Evidence(nil), run.Investigations[i].Evidence[len(run.Investigations[i].Evidence)-maxInvestigationEvidence:]...)
			run.HistoryTruncated = true
		}
		if len(run.Investigations[i].Steps) > maxInvestigationSteps {
			run.Investigations[i].Steps = append([]AgentStep(nil), run.Investigations[i].Steps[len(run.Investigations[i].Steps)-maxInvestigationSteps:]...)
			run.HistoryTruncated = true
		}
	}
	if len(run.Alerts) > maxRunAlerts {
		run.Alerts = append([]Alert(nil), run.Alerts[len(run.Alerts)-maxRunAlerts:]...)
		run.HistoryTruncated = true
	}
	for i := range run.Alerts {
		if len(run.Alerts[i].Evidence) > maxAlertEvidence {
			run.Alerts[i].Evidence = append([]Evidence(nil), run.Alerts[i].Evidence[len(run.Alerts[i].Evidence)-maxAlertEvidence:]...)
			run.HistoryTruncated = true
		}
	}
}
