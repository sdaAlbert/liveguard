package monitor

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMiniMaxTranscriberNormalizesAndIngestsTranscript(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required for the audio normalization integration test")
	}
	input := filepath.Join(t.TempDir(), "silence.wav")
	if err := os.WriteFile(input, testSilentWAV(16000), 0o600); err != nil {
		t.Fatal(err)
	}
	webm := filepath.Join(t.TempDir(), "browser-chunk.webm")
	encode := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", input, "-c:a", "libopus", webm)
	if output, err := encode.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg build cannot create a WebM/Opus fixture: %v (%s)", err, output)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("language") != "zh" {
			t.Errorf("unexpected ASR auth/language headers")
		}
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Errorf("parse ASR upload: %v", err)
			return
		}
		if r.FormValue("model") != "asr-1.0" || r.FormValue("response_format") != "json" {
			t.Errorf("unexpected ASR form values: %#v", r.Form)
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("missing normalized audio file: %v", err)
			return
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		if len(data) < 44 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
			t.Errorf("expected normalized WAV payload, got %d bytes", len(data))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"主播说，抽奖马上开始","duration":1,"trace_id":"test"}`))
	}))
	defer server.Close()
	transcriber := MiniMaxTranscriber{APIKey: "test-key", Endpoint: server.URL, FFmpegPath: ffmpeg, Client: server.Client(), Language: "zh"}
	event, err := transcriber.Transcribe(context.Background(), AudioChunk{RoomID: "room-7", Path: webm})
	if err != nil {
		t.Fatal(err)
	}
	if event.RoomID != "room-7" || event.Source != "asr" || event.Text != "主播说，抽奖马上开始" || event.Confidence != 0 {
		t.Fatalf("unexpected transcription signal: %#v", event)
	}
}

func testSilentWAV(sampleCount int) []byte {
	dataSize := sampleCount * 2
	data := make([]byte, 44+dataSize)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(36+dataSize))
	copy(data[8:12], "WAVE")
	copy(data[12:16], "fmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], 16000)
	binary.LittleEndian.PutUint32(data[28:32], 32000)
	binary.LittleEndian.PutUint16(data[32:34], 2)
	binary.LittleEndian.PutUint16(data[34:36], 16)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], uint32(dataSize))
	return data
}
