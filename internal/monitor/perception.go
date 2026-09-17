package monitor

import "context"

// AudioChunk and VideoFrame are deliberately provider-neutral. The runtime
// consumes SignalEvent values, so whisper.cpp, PaddleOCR, cloud APIs, or a
// deterministic replay can be swapped without changing the Agent.
type AudioChunk struct {
	RoomID string
	Path   string
}

type VideoFrame struct {
	RoomID string
	Path   string
}

type Transcriber interface {
	Transcribe(context.Context, AudioChunk) (SignalEvent, error)
}

type TextRecognizer interface {
	Recognize(context.Context, VideoFrame) (SignalEvent, error)
}

type SignalSource interface {
	Events(context.Context) (<-chan SignalEvent, <-chan error)
}
