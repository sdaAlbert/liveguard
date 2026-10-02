package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"liveguard/internal/agent"
)

// MiniMaxTranscriber converts a short browser-captured audio segment to a
// provider-neutral SignalEvent. Audio is normalized locally and not persisted.
type MiniMaxTranscriber struct {
	APIKey     string
	Model      string
	Endpoint   string
	Language   string
	FFmpegPath string
	Client     *http.Client
}

func (t MiniMaxTranscriber) Transcribe(ctx context.Context, chunk AudioChunk) (SignalEvent, error) {
	if strings.TrimSpace(chunk.Path) == "" {
		return SignalEvent{}, errors.New("audio chunk path is required")
	}
	if strings.TrimSpace(t.APIKey) == "" {
		return SignalEvent{}, errors.New("MiniMax ASR API key is not configured")
	}
	file, err := os.Open(chunk.Path)
	if err != nil {
		return SignalEvent{}, err
	}
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > 12<<20 {
		file.Close()
		return SignalEvent{}, errors.New("audio chunk is empty or exceeds 12 MB")
	}
	file.Close()

	ffmpeg := strings.TrimSpace(t.FFmpegPath)
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	convertCtx, cancelConvert := context.WithTimeout(ctx, 20*time.Second)
	defer cancelConvert()
	command := exec.CommandContext(convertCtx, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", chunk.Path, "-t", "20", "-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-f", "wav", "pipe:1")
	wav, err := command.Output()
	if err != nil {
		if errors.Is(convertCtx.Err(), context.DeadlineExceeded) {
			return SignalEvent{}, errors.New("audio normalization timed out")
		}
		return SignalEvent{}, fmt.Errorf("normalize audio with ffmpeg: %w", err)
	}
	if len(wav) == 0 || len(wav) > 8<<20 {
		return SignalEvent{}, errors.New("normalized audio is empty or too large")
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("model", defaultString(t.Model, "asr-1.0"))
	_ = form.WriteField("response_format", "json")
	part, err := form.CreateFormFile("file", "liveguard-audio.wav")
	if err != nil {
		return SignalEvent{}, err
	}
	if _, err := part.Write(wav); err != nil {
		return SignalEvent{}, err
	}
	if err := form.Close(); err != nil {
		return SignalEvent{}, err
	}
	endpoint := strings.TrimSpace(t.Endpoint)
	if endpoint == "" {
		endpoint = "https://api.minimaxi.com/v1/speech_to_text"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return SignalEvent{}, err
	}
	request.Header.Set("Authorization", "Bearer "+t.APIKey)
	request.Header.Set("Content-Type", form.FormDataContentType())
	if language := strings.TrimSpace(t.Language); language != "" {
		request.Header.Set("language", language)
	}
	client := t.Client
	if client == nil {
		client = agent.DefaultHTTPClient()
	}
	response, err := client.Do(request)
	if err != nil {
		return SignalEvent{}, fmt.Errorf("MiniMax speech recognition: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return SignalEvent{}, err
	}
	if response.StatusCode/100 != 2 {
		return SignalEvent{}, fmt.Errorf("MiniMax speech recognition returned %s", response.Status)
	}
	var result struct {
		Text     string  `json:"text"`
		Duration float64 `json:"duration"`
		TraceID  string  `json:"trace_id"`
	}
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return SignalEvent{}, fmt.Errorf("decode MiniMax speech recognition: %w", err)
	}
	text := strings.TrimSpace(result.Text)
	if text == "" {
		return SignalEvent{}, ErrNoSpeech
	}
	// MiniMax ASR does not return confidence for the plain JSON response. Zero
	// represents unavailable confidence; evidenceVerifier applies its fallback.
	observedAt := chunk.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	return SignalEvent{ID: chunk.EventID, RoomID: chunk.RoomID, Source: SignalASR, Text: text, ObservedAt: observedAt.UTC()}, nil
}

func defaultString(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}
