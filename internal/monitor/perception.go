package monitor

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ErrNoSpeech means a valid audio segment contained no words ASR could
// transcribe. It is a normal monitoring outcome, not a broken audio stream.
var ErrNoSpeech = errors.New("audio chunk contains no recognized speech")

// AudioChunk and VideoFrame are deliberately provider-neutral. The runtime
// consumes SignalEvent values, so whisper.cpp, PaddleOCR, cloud APIs, or a
// deterministic replay can be swapped without changing the Agent.
type AudioChunk struct {
	RoomID     string
	Path       string
	EventID    string
	ObservedAt time.Time
}

type VideoFrame struct {
	RoomID  string
	Path    string
	EventID string
}

type Transcriber interface {
	Transcribe(context.Context, AudioChunk) (SignalEvent, error)
}

type TextRecognizer interface {
	Recognize(context.Context, VideoFrame) (SignalEvent, error)
}

type SignalFeed interface {
	Events(context.Context) (<-chan SignalEvent, <-chan error)
}

// TesseractRecognizer is a local OCR adapter. It deliberately depends on the
// tesseract executable rather than a Go SDK so the Agent Runtime stays
// provider-neutral. The binary is optional; replay/manual signals still work
// when it is not configured.
type TesseractRecognizer struct {
	Binary   string
	Language string
	Timeout  time.Duration
}

func (r TesseractRecognizer) Recognize(ctx context.Context, frame VideoFrame) (SignalEvent, error) {
	if strings.TrimSpace(frame.Path) == "" {
		return SignalEvent{}, errors.New("ocr frame path is required")
	}
	binary := strings.TrimSpace(r.Binary)
	if binary == "" {
		binary = "tesseract"
	}
	language := strings.TrimSpace(r.Language)
	if language == "" {
		language = "chi_sim+eng"
	}
	if r.Timeout <= 0 {
		r.Timeout = 8 * time.Second
	}
	ocrCtx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ocrCtx, binary, frame.Path, "stdout", "-l", language, "tsv")
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ocrCtx.Err(), context.DeadlineExceeded) {
			return SignalEvent{}, fmt.Errorf("ocr timed out after %s", r.Timeout)
		}
		return SignalEvent{}, fmt.Errorf("tesseract: %w", err)
	}
	text, confidence, err := parseTesseractTSV(out)
	if err != nil {
		return SignalEvent{}, err
	}
	if text == "" {
		return SignalEvent{}, errors.New("ocr returned no text")
	}
	return SignalEvent{ID: frame.EventID, RoomID: frame.RoomID, Source: SignalOCR, Text: text, Confidence: confidence, ObservedAt: time.Now().UTC()}, nil
}

func parseTesseractTSV(output []byte) (string, float64, error) {
	reader := csv.NewReader(strings.NewReader(string(output)))
	reader.Comma = '\t'
	reader.FieldsPerRecord = -1
	var lines []string
	lineWords := map[string][]string{}
	lineOrder := make([]string, 0)
	totalConfidence := 0.0
	confidenceCount := 0
	for line := 0; ; line++ {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", 0, fmt.Errorf("parse tesseract output: %w", err)
		}
		if line == 0 || len(record) < 12 || strings.TrimSpace(record[11]) == "" {
			continue
		}
		lineKey := strings.Join(record[1:5], ":")
		if _, exists := lineWords[lineKey]; !exists {
			lineOrder = append(lineOrder, lineKey)
		}
		lineWords[lineKey] = append(lineWords[lineKey], strings.TrimSpace(record[11]))
		confidence, err := strconv.ParseFloat(strings.TrimSpace(record[10]), 64)
		if err == nil && confidence >= 0 {
			totalConfidence += confidence
			confidenceCount++
		}
	}
	for _, key := range lineOrder {
		if line := compactHanSpacing(strings.Join(lineWords[key], " ")); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return "", 0, nil
	}
	if confidenceCount == 0 {
		return strings.Join(lines, "\n"), 0.5, nil
	}
	return strings.Join(lines, "\n"), totalConfidence / float64(confidenceCount) / 100, nil
}

// Chinese OCR commonly emits one token per glyph. Compact only spaces between
// Han characters so keyword matching works without merging English words.
func compactHanSpacing(text string) string {
	runes := []rune(text)
	var result strings.Builder
	for i := 0; i < len(runes); i++ {
		if !unicode.IsSpace(runes[i]) {
			result.WriteRune(runes[i])
			continue
		}
		next := i + 1
		for next < len(runes) && unicode.IsSpace(runes[next]) {
			next++
		}
		current := []rune(result.String())
		if len(current) > 0 && next < len(runes) && unicode.Is(unicode.Han, current[len(current)-1]) && unicode.Is(unicode.Han, runes[next]) {
			i = next - 1
			continue
		}
		if len(current) > 0 && current[len(current)-1] != ' ' {
			result.WriteRune(' ')
		}
		i = next - 1
	}
	return strings.TrimSpace(result.String())
}

// StaticTextRecognizer makes the perception-to-Agent path deterministic in
// tests and local demos without pretending to be OCR.
type StaticTextRecognizer struct {
	Text       string
	Confidence float64
}

// StaticTranscriber is a deterministic adapter for API and Agent integration tests.
type StaticTranscriber struct {
	Text       string
	Confidence float64
}

func (t StaticTranscriber) Transcribe(_ context.Context, chunk AudioChunk) (SignalEvent, error) {
	if strings.TrimSpace(t.Text) == "" {
		return SignalEvent{}, errors.New("static transcript text is required")
	}
	confidence := t.Confidence
	if confidence <= 0 || confidence > 1 {
		confidence = 0.78
	}
	observedAt := chunk.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	return SignalEvent{ID: chunk.EventID, RoomID: chunk.RoomID, Source: SignalASR, Text: t.Text, Confidence: confidence, ObservedAt: observedAt.UTC()}, nil
}

func (r StaticTextRecognizer) Recognize(_ context.Context, frame VideoFrame) (SignalEvent, error) {
	if strings.TrimSpace(r.Text) == "" {
		return SignalEvent{}, errors.New("static ocr text is required")
	}
	confidence := r.Confidence
	if confidence <= 0 || confidence > 1 {
		confidence = 0.82
	}
	return SignalEvent{ID: frame.EventID, RoomID: frame.RoomID, Source: SignalOCR, Text: r.Text, Confidence: confidence, ObservedAt: time.Now().UTC()}, nil
}
