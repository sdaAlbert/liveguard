package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"liveguard/internal/browser"
	"liveguard/internal/domain"
	"liveguard/internal/monitor"
)

//go:embed static/*
var assets embed.FS

type Server struct {
	monitorService      MonitorService
	artifactDir         string
	openBrowserSession  func(string) error
	browserSessionState func() browser.SessionState
	shutdownApp         func()
	mu                  sync.Mutex
	subscribers         map[chan domain.Event]struct{}
}

type MonitorService interface {
	Start(context.Context, monitor.StartInput) (*monitor.Run, error)
	List(context.Context) ([]monitor.Summary, error)
	Get(context.Context, string) (*monitor.Run, error)
	UpdateGoal(context.Context, string, string) (*monitor.Run, error)
	AnswerInvestigation(context.Context, string, string, string) (*monitor.Run, error)
	Pause(context.Context, string) (*monitor.Run, error)
	Resume(context.Context, string) (*monitor.Run, error)
	Stop(context.Context, string) (*monitor.Run, error)
	SubmitSignal(context.Context, string, monitor.SignalEvent) error
	SubmitFrame(context.Context, string, monitor.VideoFrame) error
	SubmitAudio(context.Context, string, monitor.AudioChunk) error
}

type ModelSettingsService interface {
	ModelSettings() monitor.ModelSettingsStatus
	ConfigureModel(monitor.ModelSettingsInput) (monitor.ModelSettingsStatus, error)
}

type frameArchiveReader interface {
	ArchivedFramePath(context.Context, string, string) (string, error)
}

func New(artifactDir string) *Server {
	return &Server{artifactDir: artifactDir, subscribers: map[chan domain.Event]struct{}{}}
}

func (s *Server) SetMonitorService(service MonitorService) { s.monitorService = service }

func (s *Server) SetBrowserSession(open func(string) error, state func() browser.SessionState) {
	s.openBrowserSession = open
	s.browserSessionState = state
}

func (s *Server) SetShutdownHandler(shutdown func()) { s.shutdownApp = shutdown }

func (s *Server) Publish(event domain.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	staticFS, _ := fs.Sub(assets, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	mux.Handle("/artifacts/", http.StripPrefix("/artifacts/", http.FileServer(http.Dir(s.artifactDir))))
	mux.HandleFunc("/api/monitors", s.monitors)
	mux.HandleFunc("/api/monitors/", s.monitorRun)
	mux.HandleFunc("/api/events", s.events)
	mux.HandleFunc("/api/browser/session", s.browserSession)
	mux.HandleFunc("/api/settings/model", s.modelSettings)
	mux.HandleFunc("/api/shutdown", s.shutdown)
	mux.HandleFunc("/monitor", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "static/monitor.html")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, assets, "static/monitor.html")
	})
	return securityHeaders(otelhttp.NewHandler(mux, "liveguard.http"))
}

func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	if !remoteIsLoopback(r.RemoteAddr) {
		writeError(w, http.StatusForbidden, "退出服务仅允许本机访问")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "请使用 POST 请求退出服务")
		return
	}
	if s.shutdownApp == nil {
		writeError(w, http.StatusServiceUnavailable, "服务退出功能未启用")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"message": "LiveGuard 正在关闭"})
	s.shutdownApp()
}

func (s *Server) modelSettings(w http.ResponseWriter, r *http.Request) {
	if !remoteIsLoopback(r.RemoteAddr) {
		writeError(w, http.StatusForbidden, "模型设置仅允许本机访问")
		return
	}
	service, ok := s.monitorService.(ModelSettingsService)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "模型设置暂不可用")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, service.ModelSettings())
	case http.MethodPut:
		var input monitor.ModelSettingsInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "设置格式无效")
			return
		}
		status, err := service.ConfigureModel(input)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, status)
	default:
		w.Header().Set("Allow", "GET, PUT")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func remoteIsLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = strings.Trim(remote, "[]")
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) monitors(w http.ResponseWriter, r *http.Request) {
	if s.monitorService == nil {
		writeError(w, http.StatusServiceUnavailable, "直播信号监控未启用")
		return
	}
	switch r.Method {
	case http.MethodGet:
		runs, err := s.monitorService.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, runs)
	case http.MethodPost:
		var input monitor.StartInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "请输入直播地址和关注需求")
			return
		}
		input.TargetURL = strings.TrimSpace(input.TargetURL)
		input.Goal = strings.TrimSpace(input.Goal)
		parsed, parseErr := url.Parse(input.TargetURL)
		if parseErr != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			writeError(w, http.StatusBadRequest, "请输入有效的直播 http/https 地址")
			return
		}
		goalLength := len([]rune(input.Goal))
		if goalLength < 2 || goalLength > 240 {
			writeError(w, http.StatusBadRequest, "关注需求应为 2-240 个字符")
			return
		}
		run, err := s.monitorService.Start(r.Context(), input)
		if err != nil {
			if errors.Is(err, monitor.ErrCapacity) {
				writeError(w, http.StatusConflict, "最多同时打开 8 个监控窗口，请先停止一个")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, run)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) monitorRun(w http.ResponseWriter, r *http.Request) {
	if s.monitorService == nil {
		writeError(w, http.StatusServiceUnavailable, "直播信号监控未启用")
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/monitors/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	if len(parts) == 3 && parts[1] == "frames" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		reader, ok := s.monitorService.(frameArchiveReader)
		if !ok {
			http.NotFound(w, r)
			return
		}
		path, err := reader.ArchivedFramePath(r.Context(), id, parts[2])
		if err != nil {
			if errors.Is(err, monitor.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			writeError(w, http.StatusInternalServerError, "读取图片证据失败")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, path)
		return
	}
	if len(parts) == 2 && parts[1] == "goal" && r.Method == http.MethodPut {
		var input struct {
			Goal string `json:"goal"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "请输入新的监控需求")
			return
		}
		if length := len([]rune(strings.TrimSpace(input.Goal))); length < 2 || length > 240 {
			writeError(w, http.StatusBadRequest, "监控需求应为 2-240 个字符")
			return
		}
		run, err := s.monitorService.UpdateGoal(r.Context(), id, input.Goal)
		if err != nil {
			if errors.Is(err, monitor.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			if errors.Is(err, monitor.ErrInvalidTransition) {
				writeError(w, http.StatusConflict, "监控已停止，不能修改需求")
				return
			}
			if errors.Is(err, monitor.ErrGoalChanged) {
				writeError(w, http.StatusConflict, "需求已被更新，请检查当前需求后重试")
				return
			}
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, run)
		return
	}
	if len(parts) == 4 && parts[1] == "investigations" && parts[3] == "reply" && r.Method == http.MethodPost {
		var input struct {
			Answer string `json:"answer"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "请输入补充说明")
			return
		}
		if length := len([]rune(strings.TrimSpace(input.Answer))); length < 1 || length > 1000 {
			writeError(w, http.StatusBadRequest, "补充说明应为 1-1000 个字符")
			return
		}
		run, err := s.monitorService.AnswerInvestigation(r.Context(), id, parts[2], input.Answer)
		if err != nil {
			if errors.Is(err, monitor.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			if errors.Is(err, monitor.ErrInvestigationNotWaiting) || errors.Is(err, monitor.ErrInvalidTransition) {
				writeError(w, http.StatusConflict, "这条求助已回复、已结束或监控不在运行")
				return
			}
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, run)
		return
	}
	if len(parts) == 2 && parts[1] == "stop" && r.Method == http.MethodPost {
		run, err := s.monitorService.Stop(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, run)
		return
	}
	if len(parts) == 2 && parts[1] == "pause" && r.Method == http.MethodPost {
		run, err := s.monitorService.Pause(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, run)
		return
	}
	if len(parts) == 2 && parts[1] == "resume" && r.Method == http.MethodPost {
		run, err := s.monitorService.Resume(r.Context(), id)
		if err != nil {
			if errors.Is(err, monitor.ErrCapacity) {
				writeError(w, http.StatusTooManyRequests, "同时运行的监控已达上限")
				return
			}
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, run)
		return
	}
	if len(parts) == 2 && parts[1] == "signals" && r.Method == http.MethodPost {
		var event monitor.SignalEvent
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&event); err != nil {
			writeError(w, http.StatusBadRequest, "请输入有效的直播信号")
			return
		}
		if strings.TrimSpace(event.Text) == "" {
			writeError(w, http.StatusBadRequest, "直播信号文本不能为空")
			return
		}
		if err := s.monitorService.SubmitSignal(r.Context(), id, event); err != nil {
			if errors.Is(err, monitor.ErrPendingFull) || errors.Is(err, monitor.ErrFrameBudget) {
				writeError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			if errors.Is(err, monitor.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		run, err := s.monitorService.Get(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, run)
		return
	}
	if len(parts) == 2 && parts[1] == "ocr" && r.Method == http.MethodPost {
		roomID, imagePath, cleanup, err := saveOCRUpload(w, r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		defer cleanup()
		if err := s.monitorService.SubmitFrame(r.Context(), id, monitor.VideoFrame{RoomID: roomID, Path: imagePath, EventID: r.Header.Get("Idempotency-Key")}); err != nil {
			if errors.Is(err, monitor.ErrPendingFull) || errors.Is(err, monitor.ErrFrameBudget) {
				writeError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		run, err := s.monitorService.Get(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, run)
		return
	}
	if len(parts) == 2 && parts[1] == "audio" && r.Method == http.MethodPost {
		roomID, audioPath, cleanup, err := saveAudioUpload(w, r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		defer cleanup()
		observedAt := time.Time{}
		if value := strings.TrimSpace(r.FormValue("observed_at")); value != "" {
			observedAt, err = time.Parse(time.RFC3339Nano, value)
			if err != nil {
				writeError(w, http.StatusBadRequest, "音频片段采集时间格式无效")
				return
			}
		}
		if err := s.monitorService.SubmitAudio(r.Context(), id, monitor.AudioChunk{RoomID: roomID, Path: audioPath, EventID: r.Header.Get("Idempotency-Key"), ObservedAt: observedAt}); err != nil {
			if errors.Is(err, monitor.ErrPendingFull) || errors.Is(err, monitor.ErrFrameBudget) {
				writeError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		run, err := s.monitorService.Get(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, run)
		return
	}
	if len(parts) != 1 || r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	run, err := s.monitorService.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, monitor.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

const maxAudioChunkBytes = 5 << 20

func saveAudioUpload(w http.ResponseWriter, r *http.Request) (roomID, audioPath string, cleanup func(), err error) {
	cleanup = func() {}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return "", "", cleanup, errors.New("请上传一段直播音频")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAudioChunkBytes+(64<<10))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return "", "", cleanup, errors.New("音频片段不能超过 5MB")
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("audio")
	if err != nil {
		return "", "", cleanup, errors.New("请选择音频片段")
	}
	defer file.Close()
	roomID = strings.TrimSpace(r.FormValue("room_id"))
	extension := strings.ToLower(filepath.Ext(header.Filename))
	switch extension {
	case ".webm", ".wav", ".ogg", ".mp3", ".m4a", ".opus", ".aac", ".flac":
	default:
		extension = ".webm"
	}
	temporary, err := os.CreateTemp("", "liveguard-audio-*"+extension)
	if err != nil {
		return "", "", cleanup, err
	}
	audioPath = temporary.Name()
	cleanup = func() { _ = os.Remove(audioPath) }
	written, copyErr := io.Copy(temporary, io.LimitReader(file, maxAudioChunkBytes+1))
	closeErr := temporary.Close()
	if copyErr != nil || closeErr != nil || written <= 0 || written > maxAudioChunkBytes {
		cleanup()
		return "", "", func() {}, errors.New("音频片段无效或超过 5MB")
	}
	return roomID, audioPath, cleanup, nil
}

const maxOCRImageBytes = 8 << 20

func saveOCRUpload(w http.ResponseWriter, r *http.Request) (roomID, imagePath string, cleanup func(), err error) {
	cleanup = func() {}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return "", "", cleanup, errors.New("请上传 PNG 或 JPEG 图片")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxOCRImageBytes+(64<<10))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return "", "", cleanup, errors.New("图片不能超过 8MB，请检查文件后重试")
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, _, err := r.FormFile("image")
	if err != nil {
		return "", "", cleanup, errors.New("请选择要识别的图片")
	}
	defer file.Close()
	roomID = strings.TrimSpace(r.FormValue("room_id"))

	header := make([]byte, 512)
	n, readErr := file.Read(header)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return "", "", cleanup, errors.New("无法读取图片")
	}
	extension := map[string]string{"image/png": ".png", "image/jpeg": ".jpg"}[http.DetectContentType(header[:n])]
	if extension == "" {
		return "", "", cleanup, errors.New("目前只支持 PNG 和 JPEG 图片")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", "", cleanup, errors.New("无法读取图片")
	}
	temporary, err := os.CreateTemp("", "liveguard-ocr-*"+extension)
	if err != nil {
		return "", "", cleanup, errors.New("无法保存 OCR 图片")
	}
	imagePath = temporary.Name()
	written, copyErr := io.Copy(temporary, io.LimitReader(file, maxOCRImageBytes+1))
	closeErr := temporary.Close()
	if copyErr != nil || closeErr != nil || written > maxOCRImageBytes {
		_ = os.Remove(imagePath)
		return "", "", cleanup, errors.New("图片不能超过 8MB，或文件读取失败")
	}
	return roomID, filepath.Clean(imagePath), func() { _ = os.Remove(imagePath) }, nil
}

func (s *Server) browserSession(w http.ResponseWriter, r *http.Request) {
	if s.browserSessionState == nil || s.openBrowserSession == nil {
		writeError(w, http.StatusServiceUnavailable, "专用浏览器会话未启用")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.browserSessionState())
	case http.MethodPost:
		var input struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "请求格式无效")
			return
		}
		if input.URL = strings.TrimSpace(input.URL); input.URL == "" {
			input.URL = "https://www.douyin.com/"
		}
		if err := s.openBrowserSession(input.URL); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, s.browserSessionState())
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := make(chan domain.Event, 8)
	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.subscribers, ch); s.mu.Unlock() }()
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-ch:
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: update\ndata: %s\n\n", data)
			flusher.Flush()
		case <-time.After(15 * time.Second):
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
