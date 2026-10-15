package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// LiveCapturer keeps one visible, dedicated Chrome profile open and captures
// frames from a tab through Chrome DevTools Protocol. It never reads the user's
// everyday Chrome profile.
type LiveCapturer struct {
	ChromePath   string
	ProfileDir   string
	AllowedHosts map[string]bool
	Warmup       time.Duration
	// Headless is used by unattended browser/OCR stability fixtures.
	Headless bool

	mu          sync.Mutex
	cmd         *exec.Cmd
	port        int
	pages       map[string]*livePage
	client      *http.Client
	loginOpened bool
}

type livePage struct {
	targetID string
	conn     *websocket.Conn
	mu       sync.Mutex
	nextID   int
}

type devtoolsTarget struct {
	ID                   string `json:"id"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

type devtoolsResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// OpenLoginWindow launches the dedicated monitoring Chrome and opens a platform URL.
// The first monitor reuses this authenticated tab and navigates it to the
// requested live URL; later monitors get their own tabs in the same profile.
func (c *LiveCapturer) OpenLoginWindow(targetURL string) error {
	if err := validateLiveURL(targetURL, c.AllowedHosts); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		page, _, err := c.page(ctx, "__liveguard_login__", targetURL)
		if err != nil {
			return err
		}
		_, err = page.call(ctx, "Page.bringToFront", map[string]any{})
		if err == nil {
			c.mu.Lock()
			c.loginOpened = true
			c.mu.Unlock()
			return nil
		}
		lastErr = err
		c.discardPage("__liveguard_login__", page)
	}
	return fmt.Errorf("bring dedicated Chrome login window to front after reconnect: %w", lastErr)
}

func (c *LiveCapturer) SessionState() SessionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return SessionState{Configured: strings.TrimSpace(c.ProfileDir) != "", Open: c.cmd != nil || c.loginOpened}
}

// Shutdown terminates the dedicated browser when LiveGuard exits cleanly.
func (c *LiveCapturer) Shutdown() {
	c.mu.Lock()
	for id, page := range c.pages {
		_ = page.conn.Close()
		delete(c.pages, id)
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		c.cmd = nil
	}
	c.port = 0
	c.loginOpened = false
	c.mu.Unlock()
}

// Capture writes a screenshot of the target's live Chrome tab to destination.
// The tab and authenticated browser session are reused on later calls.
func (c *LiveCapturer) Capture(ctx context.Context, monitorID, targetURL, destination string) error {
	if err := validateLiveURL(targetURL, c.AllowedHosts); err != nil {
		return err
	}
	page, created, err := c.page(ctx, monitorID, targetURL)
	if err != nil {
		return err
	}
	if created {
		warmup := c.Warmup
		if warmup <= 0 {
			warmup = 7 * time.Second
		}
		if err := waitContext(ctx, warmup); err != nil {
			return err
		}
		// Muting the player permits autoplay where the site allows it. The page
		// remains visible so the user can complete login or verification.
		_, _ = page.call(ctx, "Runtime.evaluate", map[string]any{
			"expression": "document.querySelectorAll('video').forEach(v => { v.muted = true; v.play().catch(() => {}); })",
		})
	}
	result, err := page.call(ctx, "Page.captureScreenshot", map[string]any{"format": "png", "captureBeyondViewport": false})
	if err != nil {
		if ctx.Err() != nil || isDevToolsDisconnected(err) {
			c.discardPage(monitorID, page)
		}
		return fmt.Errorf("capture Chrome tab: %w", err)
	}
	var payload struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(result, &payload); err != nil || payload.Data == "" {
		return errors.New("Chrome returned an empty screenshot")
	}
	image, err := base64.StdEncoding.DecodeString(payload.Data)
	if err != nil {
		return fmt.Errorf("decode Chrome screenshot: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(destination, image, 0o600); err != nil {
		return err
	}
	return nil
}

// PageTitle reads the loaded page title using the same dedicated Chrome tab
// used for capture. It does not inspect the user's everyday browser profile.
func (c *LiveCapturer) PageTitle(ctx context.Context, monitorID, targetURL string) (string, error) {
	if err := validateLiveURL(targetURL, c.AllowedHosts); err != nil {
		return "", err
	}
	page, _, err := c.page(ctx, monitorID, targetURL)
	if err != nil {
		return "", err
	}
	result, err := page.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    "document.querySelector('meta[property=\"og:title\"]')?.content || document.title || ''",
		"returnByValue": true,
	})
	if err != nil {
		if ctx.Err() != nil || isDevToolsDisconnected(err) {
			c.discardPage(monitorID, page)
		}
		return "", err
	}
	var payload struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return "", fmt.Errorf("read Chrome page title: %w", err)
	}
	return payload.Result.Value, nil
}

// Close releases one monitor tab while keeping the shared login browser open.
func (c *LiveCapturer) Close(monitorID string) {
	c.mu.Lock()
	page := c.pages[monitorID]
	delete(c.pages, monitorID)
	if page != nil {
		_ = page.conn.Close()
		if c.port != 0 {
			request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/close/%s", c.port, url.PathEscape(page.targetID)), nil)
			if request != nil {
				if response, err := c.httpClient().Do(request); err == nil {
					response.Body.Close()
				}
			}
		}
	}
	c.mu.Unlock()
}

// CloseAll releases monitor tabs during process shutdown. Stopping one monitor
// leaves its tab open so the user can continue watching the live room.
func (c *LiveCapturer) CloseAll() {
	c.mu.Lock()
	ids := make([]string, 0, len(c.pages))
	for id := range c.pages {
		if id != "__liveguard_login__" {
			ids = append(ids, id)
		}
	}
	c.mu.Unlock()
	for _, id := range ids {
		c.Close(id)
	}
}

func (c *LiveCapturer) page(ctx context.Context, monitorID, targetURL string) (*livePage, bool, error) {
	for !c.mu.TryLock() {
		if err := waitContext(ctx, 5*time.Millisecond); err != nil {
			return nil, false, err
		}
	}
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if c.pages == nil {
		c.pages = map[string]*livePage{}
	}
	if page := c.pages[monitorID]; page != nil {
		return page, false, nil
	}
	if monitorID != "__liveguard_login__" {
		if loginPage := c.pages["__liveguard_login__"]; loginPage != nil {
			if _, err := loginPage.call(ctx, "Page.navigate", map[string]any{"url": targetURL}); err != nil {
				// The login tab can outlive its DevTools WebSocket. Discard the
				// stale handle and open a fresh tab in the same Chrome profile.
				delete(c.pages, "__liveguard_login__")
				_ = loginPage.conn.Close()
			} else {
				delete(c.pages, "__liveguard_login__")
				c.pages[monitorID] = loginPage
				return loginPage, true, nil
			}
		}
	}
	if err := c.startBrowser(ctx); err != nil {
		return nil, false, err
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/json/new?%s", c.port, targetURL)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, nil)
	if err != nil {
		return nil, false, err
	}
	response, err := c.httpClient().Do(request)
	if err != nil {
		return nil, false, fmt.Errorf("open live tab: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("open live tab: Chrome DevTools returned %s", response.Status)
	}
	var target devtoolsTarget
	if err := json.NewDecoder(response.Body).Decode(&target); err != nil {
		return nil, false, fmt.Errorf("read Chrome tab: %w", err)
	}
	config, err := websocket.NewConfig(target.WebSocketDebuggerURL, "http://127.0.0.1")
	if err != nil {
		return nil, false, err
	}
	connection, err := config.DialContext(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("connect to Chrome tab: %w", err)
	}
	page := &livePage{targetID: target.ID, conn: connection}
	c.pages[monitorID] = page
	if _, err := page.call(ctx, "Page.enable", map[string]any{}); err != nil {
		page.conn.Close()
		delete(c.pages, monitorID)
		return nil, false, err
	}
	return page, true, nil
}

func (c *LiveCapturer) discardPage(monitorID string, page *livePage) {
	c.mu.Lock()
	if c.pages[monitorID] == page {
		delete(c.pages, monitorID)
	}
	port := c.port
	c.mu.Unlock()
	if page != nil && page.conn != nil {
		_ = page.conn.Close()
	}
	if page != nil && port != 0 && page.targetID != "" {
		request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/close/%s", port, url.PathEscape(page.targetID)), nil)
		if request != nil {
			if response, err := c.httpClient().Do(request); err == nil {
				response.Body.Close()
			}
		}
	}
}

func isDevToolsDisconnected(err error) bool {
	if err == nil {
		return false
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"connection was aborted", "use of closed network connection", "wsasend", "wsarecv", "unexpected eof", "broken pipe"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (c *LiveCapturer) startBrowser(ctx context.Context) error {
	if c.cmd != nil && c.cmd.Process != nil {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/version", c.port), nil)
		if response, err := c.httpClient().Do(request); err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		c.cmd = nil
		c.port = 0
	}
	if strings.TrimSpace(c.ProfileDir) == "" {
		return errors.New("dedicated Chrome profile is not configured")
	}
	profile, err := filepath.Abs(c.ProfileDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return err
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	binary := strings.TrimSpace(c.ChromePath)
	if binary == "" {
		binary = "chrome"
	}
	arguments := []string{
		"--new-window", "--no-first-run", "--no-default-browser-check",
		"--remote-debugging-address=127.0.0.1", "--remote-debugging-port=" + strconv.Itoa(port),
		"--remote-allow-origins=*",
		"--user-data-dir=" + profile, "--autoplay-policy=no-user-gesture-required",
		"--window-size=1440,1000", "about:blank",
		"--disable-background-timer-throttling", "--disable-renderer-backgrounding", "--disable-backgrounding-occluded-windows",
	}
	if c.Headless {
		arguments = append(arguments, "--headless=new")
	}
	command := exec.Command(binary, arguments...)
	if err := command.Start(); err != nil {
		return fmt.Errorf("start dedicated Chrome: %w", err)
	}
	c.cmd, c.port = command, port
	go func() { _ = command.Wait() }()
	deadline := time.NewTimer(10 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
		response, requestErr := c.httpClient().Do(request)
		if requestErr == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			_ = command.Process.Kill()
			c.cmd, c.port = nil, 0
			return ctx.Err()
		case <-deadline.C:
			_ = command.Process.Kill()
			c.cmd, c.port = nil, 0
			return errors.New("Chrome DevTools did not start within 10 seconds")
		case <-ticker.C:
		}
	}
}

func (c *LiveCapturer) httpClient() *http.Client {
	if c.client == nil {
		c.client = &http.Client{Timeout: 3 * time.Second}
	}
	return c.client
}

func (p *livePage) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	// A visual tool can share this page with sampling. Waiting for that call
	// must consume the caller's budget rather than block behind a stale socket.
	for !p.mu.TryLock() {
		if err := waitContext(ctx, 5*time.Millisecond); err != nil {
			return nil, err
		}
	}
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.nextID++
	request := struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{ID: p.nextID, Method: method, Params: params}
	deadline := time.Now().Add(30 * time.Second)
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	_ = p.conn.SetDeadline(deadline)
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = p.conn.SetDeadline(time.Now())
		close(cancelled)
	})
	defer func() {
		if !stop() {
			<-cancelled
		}
	}()
	if err := websocket.JSON.Send(p.conn, request); err != nil {
		return nil, err
	}
	for {
		var response devtoolsResponse
		if err := websocket.JSON.Receive(p.conn, &response); err != nil {
			return nil, err
		}
		if response.ID != request.ID {
			continue // Ignore asynchronous Page events.
		}
		if response.Error != nil {
			return nil, errors.New(response.Error.Message)
		}
		return response.Result, nil
	}
}

func validateLiveURL(raw string, allowedHosts map[string]bool) error {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return errors.New("live monitor URL must be a valid http/https URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if len(allowedHosts) == 0 {
		return nil
	}
	for allowed := range allowedHosts {
		allowed = strings.ToLower(allowed)
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return nil
		}
	}
	return fmt.Errorf("live monitor host %q is not allowed", host)
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
