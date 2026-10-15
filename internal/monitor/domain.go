package monitor

import "time"

type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunPaused    RunStatus = "paused"
	RunCompleted RunStatus = "completed"
	RunStopped   RunStatus = "stopped"
	RunFailed    RunStatus = "failed"
)

type SourceHealth string

const (
	SourceUnconfigured SourceHealth = "unconfigured"
	SourceWaiting      SourceHealth = "waiting_signal"
	SourceHealthy      SourceHealth = "healthy"
	SourceDelayed      SourceHealth = "delayed"
	SourceNeedsLogin   SourceHealth = "needs_login"
	SourceFailed       SourceHealth = "capture_failed"
)

type SignalSource string

const (
	SignalOCR    SignalSource = "ocr"
	SignalASR    SignalSource = "asr"
	SignalVision SignalSource = "vision"
	// Frame checks are created only by browser capture, never by HTTP uploads.
	SignalFrame SignalSource = "frame"
)

type BaselineStatus string

const (
	BaselinePending     BaselineStatus = "pending"
	BaselineReviewing   BaselineStatus = "reviewing"
	BaselineReady       BaselineStatus = "ready"
	BaselineNeedsLogin  BaselineStatus = "needs_login"
	BaselineUnavailable BaselineStatus = "unavailable"
)

// LiveBaseline is the one-time, textual profile produced from the first live
// frame. The source image is temporary and is never stored in this record.
type LiveBaseline struct {
	Status                         BaselineStatus `json:"status"`
	StreamType                     string         `json:"stream_type,omitempty"`
	Summary                        string         `json:"summary,omitempty"`
	VisibleRegions                 []string       `json:"visible_regions,omitempty"`
	SuggestedReviewIntervalSeconds int            `json:"suggested_review_interval_seconds,omitempty"`
	RecommendVision                bool           `json:"recommend_vision,omitempty"`
	RecommendASR                   bool           `json:"recommend_asr,omitempty"`
	Reason                         string         `json:"reason,omitempty"`
	Model                          string         `json:"model,omitempty"`
	InputTokens                    int64          `json:"input_tokens,omitempty"`
	OutputTokens                   int64          `json:"output_tokens,omitempty"`
	LatencyMS                      int64          `json:"latency_ms,omitempty"`
	CheckedAt                      time.Time      `json:"checked_at,omitempty"`
	Error                          string         `json:"error,omitempty"`
}

type Room struct {
	ID                   string       `json:"id"`
	Name                 string       `json:"name"`
	Platform             string       `json:"platform,omitempty"`
	Status               string       `json:"status"`
	LastSeen             time.Time    `json:"last_seen,omitempty"`
	LastCaptureAt        time.Time    `json:"last_capture_at,omitempty"`
	CaptureIntervalMS    int64        `json:"capture_interval_ms,omitempty"`
	CaptureDurationMS    int64        `json:"capture_duration_ms,omitempty"`
	PerceptionDurationMS int64        `json:"perception_duration_ms,omitempty"`
	LastError            string       `json:"last_error,omitempty"`
	Health               SourceHealth `json:"health"`
}

type SignalEvent struct {
	GoalVersion          uint64       `json:"goal_version,omitempty"`
	ParentEventID        string       `json:"parent_event_id,omitempty"`
	ID                   string       `json:"id"`
	RoomID               string       `json:"room_id"`
	RoomName             string       `json:"room_name"`
	Source               SignalSource `json:"source"`
	Text                 string       `json:"text"`
	Confidence           float64      `json:"confidence"`
	TextRegions          []TextRegion `json:"text_regions,omitempty"`
	ObservedAt           time.Time    `json:"observed_at"`
	ValidUntil           time.Time    `json:"valid_until,omitempty"`
	ReceivedAt           time.Time    `json:"received_at,omitempty"`
	Sequence             uint64       `json:"sequence,omitempty"`
	PerceptionDurationMS int64        `json:"perception_duration_ms,omitempty"`
	CaptureDurationMS    int64        `json:"capture_duration_ms,omitempty"`
	FramePath            string       `json:"-"`
	FrameArchiveID       string       `json:"frame_archive_id,omitempty"`
}

// TextRegion preserves OCR line geometry and screen-area context for downstream
// review. Area is a geometric hint, not proof that text belongs to chat.
type TextRegion struct {
	Area       string  `json:"area,omitempty"`
	Text       string  `json:"text"`
	X          int     `json:"x"`
	Y          int     `json:"y"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	Confidence float64 `json:"confidence"`
}

type EventReceipt struct {
	ID          string    `json:"id"`
	Fingerprint string    `json:"fingerprint"`
	ReceivedAt  time.Time `json:"received_at"`
}

type Evidence struct {
	ValidUntil     time.Time    `json:"valid_until,omitempty"`
	RoomID         string       `json:"room_id,omitempty"`
	GoalVersion    uint64       `json:"goal_version,omitempty"`
	FrameArchiveID string       `json:"frame_archive_id,omitempty"`
	TextRegions    []TextRegion `json:"text_regions,omitempty"`
	EventID        string       `json:"event_id"`
	Source         string       `json:"source"`
	Text           string       `json:"text"`
	Confidence     float64      `json:"confidence"`
	ObservedAt     time.Time    `json:"observed_at"`
}

type AgentStep struct {
	ID         string    `json:"id"`
	Sequence   int       `json:"sequence"`
	Tool       string    `json:"tool"`
	Purpose    string    `json:"purpose"`
	Outcome    string    `json:"outcome"`
	CreatedAt  time.Time `json:"created_at"`
	Succeeded  *bool     `json:"succeeded,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
}

type ReviewMetrics struct {
	TransportRetries int               `json:"transport_retries,omitempty"`
	SamplingProposal *SamplingProposal `json:"sampling_proposal,omitempty"`
	SamplingPolicy   SamplingState     `json:"sampling_policy"`
	Model            string            `json:"model,omitempty"`
	InputTokens      int64             `json:"input_tokens"`
	OutputTokens     int64             `json:"output_tokens"`
	LatencyMS        int64             `json:"latency_ms"`
	Turns            int               `json:"turns"`
	StopReason       string            `json:"stop_reason,omitempty"`
	FormatRepairs    int               `json:"format_repairs,omitempty"`
}

type Investigation struct {
	GoalVersion  uint64         `json:"goal_version"`
	UserQuestion string         `json:"user_question,omitempty"`
	UserOptions  []string       `json:"user_options,omitempty"`
	UserAnswer   string         `json:"user_answer,omitempty"`
	ID           string         `json:"id"`
	RoomID       string         `json:"room_id"`
	RoomName     string         `json:"room_name"`
	EventType    string         `json:"event_type"`
	Status       string         `json:"status"`
	Conclusion   string         `json:"conclusion,omitempty"`
	Reason       string         `json:"reason,omitempty"`
	Evidence     []Evidence     `json:"evidence,omitempty"`
	Steps        []AgentStep    `json:"steps"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	Review       *ReviewMetrics `json:"review,omitempty"`
}

type Alert struct {
	EvidenceBasis   string     `json:"evidence_basis,omitempty"`
	SignalToAlertMS int64      `json:"signal_to_alert_ms"`
	ID              string     `json:"id"`
	RoomID          string     `json:"room_id"`
	RoomName        string     `json:"room_name"`
	EventType       string     `json:"event_type"`
	Priority        string     `json:"priority"`
	Summary         string     `json:"summary"`
	Confidence      float64    `json:"confidence"`
	DedupeKey       string     `json:"dedupe_key"`
	Investigation   string     `json:"investigation_id"`
	Evidence        []Evidence `json:"evidence"`
	CreatedAt       time.Time  `json:"created_at"`
}

// MonitoringPlan is the inspectable execution policy derived from the user's
// natural-language goal. Runtime limits are normalized and enforced server-side.
type MonitoringPlan struct {
	ModelCapabilities      *ModelCapabilities `json:"model_capabilities,omitempty"`
	CapabilityWarning      string             `json:"capability_warning,omitempty"`
	Summary                string             `json:"summary"`
	CaptureIntervalSeconds int                `json:"capture_interval_seconds"`
	ReviewIntervalSeconds  int                `json:"review_interval_seconds"`
	UseOCR                 bool               `json:"use_ocr"`
	RecommendASR           bool               `json:"recommend_asr"`
	VisionPolicy           string             `json:"vision_policy"`
	AlertScope             string             `json:"alert_scope"`
	Tools                  []string           `json:"tools"`
	UserConsent            []string           `json:"user_consent,omitempty"`
	Planner                string             `json:"planner"`
	Signals                []string           `json:"signals"`
	FormatRepairs          int                `json:"format_repairs,omitempty"`
}

type Run struct {
	StopReason              string          `json:"stop_reason,omitempty"`
	LastError               string          `json:"last_error,omitempty"`
	Pending                 *PendingStatus  `json:"pending,omitempty"`
	ID                      string          `json:"id"`
	Name                    string          `json:"name"`
	TargetURL               string          `json:"target_url"`
	Goal                    string          `json:"goal"`
	UserInstructions        []string        `json:"user_instructions,omitempty"`
	GoalVersion             uint64          `json:"goal_version"`
	GoalUpdatedAt           time.Time       `json:"goal_updated_at"`
	Plan                    MonitoringPlan  `json:"plan"`
	Baseline                *LiveBaseline   `json:"baseline,omitempty"`
	Sampling                SamplingState   `json:"sampling"`
	SourceMode              string          `json:"source_mode"`
	Status                  RunStatus       `json:"status"`
	Rooms                   []Room          `json:"rooms"`
	Events                  []SignalEvent   `json:"events"`
	EventReceipts           []EventReceipt  `json:"event_receipts,omitempty"`
	EventSequence           uint64          `json:"event_sequence,omitempty"`
	Investigations          []Investigation `json:"investigations"`
	Alerts                  []Alert         `json:"alerts"`
	StartedAt               time.Time       `json:"started_at"`
	UpdatedAt               time.Time       `json:"updated_at"`
	CompletedAt             *time.Time      `json:"completed_at,omitempty"`
	Version                 int64           `json:"version"`
	HistoryTruncated        bool            `json:"history_truncated,omitempty"`
	ReviewBackpressureCount int             `json:"review_backpressure_count,omitempty"`
}

type Summary struct {
	StopReason     string    `json:"stop_reason,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
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
	summary.StopReason, summary.LastError = r.StopReason, r.LastError
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
		case "needs_human", InvestigationWaitingUser:
			summary.NeedsHuman++
		}
	}
	return summary
}
