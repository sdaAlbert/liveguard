package monitor

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ErrNoSpeech means a valid audio segment contained no words ASR could
// transcribe. It is a normal monitoring outcome, not a broken audio stream.
var ErrNoSpeech = errors.New("audio chunk contains no recognized speech")

var ErrNoText = errors.New("ocr returned no text")

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
	RoomID    string
	Path      string
	EventID   string
	FocusChat bool
}

type Transcriber interface {
	Transcribe(context.Context, AudioChunk) (SignalEvent, error)
}

type TextRecognizer interface {
	Recognize(context.Context, VideoFrame) (SignalEvent, error)
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
	ocrPath := frame.Path
	xOffset := 0
	cleanup := func() {}
	if frame.FocusChat {
		if croppedPath, offset, cropCleanup, cropErr := cropRightChatPanel(frame.Path); cropErr == nil {
			ocrPath, xOffset, cleanup = croppedPath, offset, cropCleanup
		}
	}
	defer cleanup()
	cmd := exec.CommandContext(ocrCtx, binary, ocrPath, "stdout", "-l", language, "tsv")
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ocrCtx.Err(), context.DeadlineExceeded) {
			return SignalEvent{}, fmt.Errorf("ocr timed out after %s", r.Timeout)
		}
		return SignalEvent{}, fmt.Errorf("tesseract: %w", err)
	}
	text, confidence, regions, err := parseTesseractTSVWithRegions(out)
	if err != nil {
		return SignalEvent{}, err
	}
	if text == "" {
		return SignalEvent{}, ErrNoText
	}
	if frame.FocusChat && xOffset > 0 {
		for i := range regions {
			regions[i].X += xOffset
			regions[i].Area = "chat"
		}
	} else if width := pngFrameWidth(frame.Path); width > 0 {
		text, regions = prioritizeRightSideText(text, regions, width)
	}
	return SignalEvent{ID: frame.EventID, RoomID: frame.RoomID, Source: SignalOCR, Text: text, Confidence: confidence, TextRegions: regions, ObservedAt: time.Now().UTC()}, nil
}

func pngFrameWidth(path string) int {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()
	config, err := png.DecodeConfig(file)
	if err != nil {
		return 0
	}
	return config.Width
}

func prioritizeRightSideText(text string, regions []TextRegion, frameWidth int) (string, []TextRegion) {
	if frameWidth <= 0 || len(regions) == 0 {
		return text, regions
	}
	lines := strings.Split(text, "\n")
	if len(lines) != len(regions) {
		return text, regions
	}
	var right, rest []string
	for i := range regions {
		if regions[i].X+regions[i].Width/2 >= frameWidth*2/3 {
			regions[i].Area = "right"
			right = append(right, lines[i])
		} else {
			rest = append(rest, lines[i])
		}
	}
	return strings.Join(append(right, rest...), "\n"), regions
}

// cropRightChatPanel focuses OCR on Douyin's right-side chat column while the
// archived source frame remains full-size for later visual confirmation.
func cropRightChatPanel(path string) (string, int, func(), error) {
	source, err := os.Open(path)
	if err != nil {
		return "", 0, func() {}, err
	}
	frame, err := png.Decode(source)
	_ = source.Close()
	if err != nil {
		return "", 0, func() {}, err
	}
	bounds := frame.Bounds()
	width := bounds.Dx()
	if width < 480 {
		return "", 0, func() {}, errors.New("frame is too narrow to isolate the chat column")
	}
	left := bounds.Min.X + width*2/3
	cropBounds := image.Rect(left, bounds.Min.Y, bounds.Max.X, bounds.Max.Y)
	cropped := image.NewRGBA(image.Rect(0, 0, cropBounds.Dx(), cropBounds.Dy()))
	draw.Draw(cropped, cropped.Bounds(), frame, cropBounds.Min, draw.Src)
	temporary, err := os.CreateTemp("", "liveguard-chat-ocr-*.png")
	if err != nil {
		return "", 0, func() {}, err
	}
	path = temporary.Name()
	if err = png.Encode(temporary, cropped); err != nil {
		_ = temporary.Close()
		_ = os.Remove(path)
		return "", 0, func() {}, err
	}
	if err = temporary.Close(); err != nil {
		_ = os.Remove(path)
		return "", 0, func() {}, err
	}
	return path, left - bounds.Min.X, func() { _ = os.Remove(path) }, nil
}

func parseTesseractTSV(output []byte) (string, float64, error) {
	text, confidence, _, err := parseTesseractTSVWithRegions(output)
	return text, confidence, err
}

func parseTesseractTSVWithRegions(output []byte) (string, float64, []TextRegion, error) {
	reader := csv.NewReader(strings.NewReader(string(output)))
	reader.Comma = '\t'
	reader.FieldsPerRecord = -1
	type regionBuilder struct {
		words                    []string
		left, top, right, bottom int
		confidenceSum            float64
		confidenceCount          int
	}
	regionsByLine := map[string]*regionBuilder{}
	lineOrder := make([]string, 0)
	totalConfidence := 0.0
	confidenceCount := 0
	for line := 0; ; line++ {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", 0, nil, fmt.Errorf("parse tesseract output: %w", err)
		}
		if line == 0 || len(record) < 12 || strings.TrimSpace(record[11]) == "" {
			continue
		}
		lineKey := strings.Join(record[1:5], ":")
		region, exists := regionsByLine[lineKey]
		if !exists {
			lineOrder = append(lineOrder, lineKey)
			region = &regionBuilder{left: int(^uint(0) >> 1), top: int(^uint(0) >> 1)}
			regionsByLine[lineKey] = region
		}
		region.words = append(region.words, strings.TrimSpace(record[11]))
		left, leftErr := strconv.Atoi(strings.TrimSpace(record[6]))
		top, topErr := strconv.Atoi(strings.TrimSpace(record[7]))
		width, widthErr := strconv.Atoi(strings.TrimSpace(record[8]))
		height, heightErr := strconv.Atoi(strings.TrimSpace(record[9]))
		if leftErr == nil && topErr == nil && widthErr == nil && heightErr == nil && width > 0 && height > 0 {
			region.left = min(region.left, left)
			region.top = min(region.top, top)
			region.right = max(region.right, left+width)
			region.bottom = max(region.bottom, top+height)
		}
		confidence, err := strconv.ParseFloat(strings.TrimSpace(record[10]), 64)
		if err == nil && confidence >= 0 {
			totalConfidence += confidence
			confidenceCount++
			region.confidenceSum += confidence
			region.confidenceCount++
		}
	}
	var lines []string
	regions := make([]TextRegion, 0, min(len(lineOrder), 256))
	for _, key := range lineOrder {
		region := regionsByLine[key]
		line := compactHanSpacing(strings.Join(region.words, " "))
		if line != "" {
			lines = append(lines, line)
			if region.right > region.left && region.bottom > region.top && len(regions) < 256 {
				confidence := .5
				if region.confidenceCount > 0 {
					confidence = region.confidenceSum / float64(region.confidenceCount) / 100
				}
				regions = append(regions, TextRegion{Text: line, X: region.left, Y: region.top, Width: region.right - region.left, Height: region.bottom - region.top, Confidence: confidence})
			}
		}
	}
	if len(lines) == 0 {
		return "", 0, nil, nil
	}
	if confidenceCount == 0 {
		return strings.Join(lines, "\n"), 0.5, regions, nil
	}
	return strings.Join(lines, "\n"), totalConfidence / float64(confidenceCount) / 100, regions, nil
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
