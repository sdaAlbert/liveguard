package monitor

import "time"

type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunStopped   RunStatus = "stopped"
	RunFailed    RunStatus = "failed"
)

type Room struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Status   string    `json:"status"`
	LastSeen time.Time `json:"last_seen,omitempty"`
}

type SignalEvent struct {
	ID         string    `json:"id"`
	RoomID     string    `json:"room_id"`
	RoomName   string    `json:"room_name"`
	Source     string    `json:"source"`
	Text       string    `json:"text"`
	Confidence float64   `json:"confidence"`
	ObservedAt time.Time `json:"observed_at"`
}

type Evidence struct {
	EventID    string    `json:"event_id"`
	Source     string    `json:"source"`
	Text       string    `json:"text"`
	Confidence float64   `json:"confidence"`
	ObservedAt time.Time `json:"observed_at"`
}

type AgentStep struct {
	ID        string    `json:"id"`
	Sequence  int       `json:"sequence"`
	Tool      string    `json:"tool"`
	Purpose   string    `json:"purpose"`
	Outcome   string    `json:"outcome"`
	CreatedAt time.Time `json:"created_at"`
}

type Investigation struct {
	ID         string      `json:"id"`
	RoomID     string      `json:"room_id"`
	RoomName   string      `json:"room_name"`
	EventType  string      `json:"event_type"`
	Status     string      `json:"status"`
	Conclusion string      `json:"conclusion,omitempty"`
	Evidence   []Evidence  `json:"evidence,omitempty"`
	Steps      []AgentStep `json:"steps"`
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at"`
}

type Alert struct {
	ID            string     `json:"id"`
	RoomID        string     `json:"room_id"`
	RoomName      string     `json:"room_name"`
	EventType     string     `json:"event_type"`
	Priority      string     `json:"priority"`
	Summary       string     `json:"summary"`
	Confidence    float64    `json:"confidence"`
	DedupeKey     string     `json:"dedupe_key"`
	Investigation string     `json:"investigation_id"`
	Evidence      []Evidence `json:"evidence"`
	CreatedAt     time.Time  `json:"created_at"`
}

type Run struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	TargetURL      string          `json:"target_url"`
	Goal           string          `json:"goal"`
	SourceMode     string          `json:"source_mode"`
	Status         RunStatus       `json:"status"`
	Rooms          []Room          `json:"rooms"`
	Events         []SignalEvent   `json:"events"`
	Investigations []Investigation `json:"investigations"`
	Alerts         []Alert         `json:"alerts"`
	StartedAt      time.Time       `json:"started_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
	Version        int64           `json:"version"`
}

type Summary struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	TargetURL      string    `json:"target_url"`
	Goal           string    `json:"goal"`
	SourceMode     string    `json:"source_mode"`
	Status         RunStatus `json:"status"`
	Rooms          int       `json:"rooms"`
	Events         int       `json:"events"`
	Candidates     int       `json:"candidates"`
	Alerts         int       `json:"alerts"`
	Suppressed     int       `json:"suppressed"`
	NeedsHuman     int       `json:"needs_human"`
	AgentToolCalls int       `json:"agent_tool_calls"`
	LatestSignal   string    `json:"latest_signal,omitempty"`
	LatestAlert    string    `json:"latest_alert,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (r *Run) Summary() Summary {
	summary := Summary{ID: r.ID, Name: r.Name, TargetURL: r.TargetURL, Goal: r.Goal, SourceMode: r.SourceMode, Status: r.Status, Rooms: len(r.Rooms), Events: len(r.Events), Candidates: len(r.Investigations), Alerts: len(r.Alerts), StartedAt: r.StartedAt, UpdatedAt: r.UpdatedAt}
	if len(r.Events) > 0 {
		summary.LatestSignal = r.Events[len(r.Events)-1].Text
	}
	if len(r.Alerts) > 0 {
		summary.LatestAlert = r.Alerts[len(r.Alerts)-1].Summary
	}
	for _, investigation := range r.Investigations {
		summary.AgentToolCalls += len(investigation.Steps)
		switch investigation.Status {
		case "suppressed":
			summary.Suppressed++
		case "needs_human":
			summary.NeedsHuman++
		}
	}
	return summary
}
