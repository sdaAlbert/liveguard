package monitor

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
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
	// Bound CPU use per subprocess; several live rooms otherwise each launch
	// Tesseract's own OpenMP pool and compete with Chrome for every core.
	cmd.Env = append(os.Environ(), "OMP_THREAD_LIMIT=1")
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
	if width, height := pngFrameSize(frame.Path); width > 0 && height > 0 {
		text, regions = labelOCRAreas(text, regions, width, height)
	}
	return SignalEvent{ID: frame.EventID, RoomID: frame.RoomID, Source: SignalOCR, Text: text, Confidence: confidence, TextRegions: regions, ObservedAt: time.Now().UTC()}, nil
}

func pngFrameSize(path string) (int, int) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	config, err := png.DecodeConfig(file)
	if err != nil {
		return 0, 0
	}
	return config.Width, config.Height
}

// labelOCRAreas keeps OCR from the whole frame and uses geometry only to mark
// likely chat lines. The lower-right band is where scrolling comments appear;
// fixed notices higher in the right rail remain main-screen evidence.
func labelOCRAreas(text string, regions []TextRegion, frameWidth, frameHeight int) (string, []TextRegion) {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		area := "main"
		if i < len(regions) {
			region := &regions[i]
			centerX := region.X + region.Width/2
			centerY := region.Y + region.Height/2
			if centerX >= frameWidth*2/3 && centerY >= frameHeight*2/5 {
				area = "chat"
			}
			region.Area = area
		}
		label := "《主屏幕》"
		if area == "chat" {
			label = "《聊天区》"
		}
		lines[i] = label + " " + line
		if i < len(regions) {
			regions[i].Text = lines[i]
		}
	}
	return strings.Join(lines, "\n"), regions
}

func parseTesseractTSV(output []byte) (string, float64, error) {
	text, confidence, _, err := parseTesseractTSVWithRegions(output)
	return text, confidence, err
}

func parseTesseractTSVWithRegions(output []byte) (string, float64, []TextRegion, error) {
	reader := csv.NewReader(strings.NewReader(string(output)))
	reader.Comma = '\t'
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
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
			region.confidenceSum += confidence
			region.confidenceCount++
		}
	}
	var lines []string
	regions := make([]TextRegion, 0, min(len(lineOrder), 256))
	for _, key := range lineOrder {
		region := regionsByLine[key]
		line, contentRunes, noisyRunes := filterOCRLine(strings.Join(region.words, " "))
		line = compactHanSpacing(line)
		if line != "" && hasOCRWordCharacter(line) {
			confidence := .5
			if region.confidenceCount > 0 {
				confidence = region.confidenceSum / float64(region.confidenceCount) / 100
			}
			// OCR confidence is word-derived. Drop a line only when it is both
			// dominated by unsupported glyphs or exceptionally low-confidence;
			// short chat messages remain eligible because they can be meaningful.
			if (noisyRunes > 0 && noisyRunes*2 >= contentRunes) || (confidence < .12 && runeCount(line) > 3) {
				continue
			}
			lines = append(lines, line)
			totalConfidence += confidence
			confidenceCount++
			if len(regions) < 256 {
				x, y, width, height := 0, 0, 0, 0
				if region.right > region.left && region.bottom > region.top {
					x, y, width, height = region.left, region.top, region.right-region.left, region.bottom-region.top
				}
				regions = append(regions, TextRegion{Text: line, X: x, Y: y, Width: width, Height: height, Confidence: confidence})
			}
		}
	}
	if len(lines) == 0 {
		return "", 0, nil, nil
	}
	if confidenceCount == 0 {
		return strings.Join(lines, "\n"), 0.5, regions, nil
	}
	return strings.Join(lines, "\n"), totalConfidence / float64(confidenceCount), regions, nil
}

// filterOCRLine keeps Chinese/English text, digits and ordinary punctuation.
// Other scripts and decorative symbols are removed; a line mostly made from
// those characters is discarded by the caller. This intentionally does not
// try to judge whether a sequence of valid Chinese/English letters is
// semantically coherent, since usernames, names and product terms are open-ended.
func filterOCRLine(text string) (clean string, contentRunes, noisyRunes int) {
	var result strings.Builder
	for _, r := range text {
		if unicode.IsSpace(r) {
			result.WriteRune(r)
			continue
		}
		contentRunes++
		supported := unicode.Is(unicode.Han, r) || unicode.Is(unicode.Latin, r) || unicode.IsDigit(r) || unicode.IsPunct(r)
		if supported || strings.ContainsRune("￥¥€£$%‰+-−×÷=<>@#&*/\\_|~^°℃", r) {
			result.WriteRune(r)
			continue
		}
		noisyRunes++
	}
	return strings.TrimSpace(result.String()), contentRunes, noisyRunes
}

func hasOCRWordCharacter(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Latin, r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
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
