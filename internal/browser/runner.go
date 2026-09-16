package browser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"liveguard/internal/domain"
)

var (
	titlePattern  = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	scriptPattern = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	tagPattern    = regexp.MustCompile(`(?s)<[^>]+>`)
	spacePattern  = regexp.MustCompile(`\s+`)
)

type Result struct {
	Title      string
	FinalURL   string
	BodyText   string
	Screenshot string
}

type Runner struct {
	ChromePath        string
	ProfileDir        string
	SessionProfileDir string
	ArtifactDir       string
	Headless          bool
	AllowedHosts      map[string]bool
	sessionMu         sync.Mutex
	sessionUseMu      sync.Mutex
	sessionOpen       bool
	sessionInspecting bool
}

type SessionState struct {
	Configured bool `json:"configured"`
	Open       bool `json:"open"`
	Inspecting bool `json:"inspecting"`
}

func (r *Runner) OpenSession(targetURL string) error {
	if err := r.validateURL(targetURL); err != nil {
		return err
	}
	if strings.TrimSpace(r.SessionProfileDir) == "" {
		return errors.New("专用登录会话未配置")
	}
	profile, err := filepath.Abs(r.SessionProfileDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return err
	}
	r.sessionMu.Lock()
	if r.sessionOpen {
		r.sessionMu.Unlock()
		return errors.New("专用登录窗口已打开；请在该窗口完成登录")
	}
	if r.sessionInspecting {
		r.sessionMu.Unlock()
		return errors.New("专用登录态正在执行巡检，请稍后再打开登录窗口")
	}
	command := exec.Command(r.ChromePath,
		"--new-window",
		"--no-first-run",
		"--no-default-browser-check",
		"--user-data-dir="+profile,
		targetURL,
	)
	if err := command.Start(); err != nil {
		r.sessionMu.Unlock()
		return fmt.Errorf("启动 Chrome 登录窗口: %w", err)
	}
	r.sessionOpen = true
	r.sessionMu.Unlock()
	go func() {
		_ = command.Wait()
		r.sessionMu.Lock()
		r.sessionOpen = false
		r.sessionMu.Unlock()
	}()
	return nil
}

func (r *Runner) SessionState() SessionState {
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	return SessionState{Configured: strings.TrimSpace(r.SessionProfileDir) != "", Open: r.sessionOpen, Inspecting: r.sessionInspecting}
}

func (r *Runner) Inspect(ctx context.Context, taskID, targetURL string, useAuthenticatedSession bool) (Result, error) {
	if err := r.validateURL(targetURL); err != nil {
		return Result{}, err
	}
	taskProfile := filepath.Join(r.ProfileDir, taskID)
	if err := os.MkdirAll(taskProfile, 0o700); err != nil {
		return Result{}, err
	}
	taskArtifacts := filepath.Join(r.ArtifactDir, taskID)
	if err := os.MkdirAll(taskArtifacts, 0o755); err != nil {
		return Result{}, err
	}
	var visible *exec.Cmd
	if !r.Headless && !useAuthenticatedSession {
		visible = exec.CommandContext(ctx, r.ChromePath,
			"--new-window",
			"--no-first-run",
			"--no-default-browser-check",
			"--user-data-dir="+filepath.Join(taskProfile, "visible"),
			targetURL,
		)
		if err := visible.Start(); err == nil && visible.Process != nil {
			defer func() { _ = visible.Process.Kill() }()
		}
	}
	screenshotPath, err := filepath.Abs(filepath.Join(taskArtifacts, "overview.png"))
	if err != nil {
		return Result{}, err
	}
	inspectionProfilePath := filepath.Join(taskProfile, "inspection")
	if useAuthenticatedSession {
		r.sessionUseMu.Lock()
		defer r.sessionUseMu.Unlock()
		r.sessionMu.Lock()
		if strings.TrimSpace(r.SessionProfileDir) == "" {
			r.sessionMu.Unlock()
			return Result{}, errors.New("专用登录会话未配置")
		}
		if r.sessionOpen {
			r.sessionMu.Unlock()
			return Result{}, errors.New("请先在专用窗口完成登录并关闭窗口，再启动巡检")
		}
		r.sessionInspecting = true
		r.sessionMu.Unlock()
		defer func() {
			r.sessionMu.Lock()
			r.sessionInspecting = false
			r.sessionMu.Unlock()
		}()
		inspectionProfilePath = r.SessionProfileDir
	}
	inspectionProfile, err := filepath.Abs(inspectionProfilePath)
	if err != nil {
		return Result{}, err
	}
	stdout := &limitedBuffer{limit: 4 << 20}
	stderr := &limitedBuffer{limit: 64 << 10}
	browserCtx, cancelBrowser := context.WithTimeout(ctx, 25*time.Second)
	defer cancelBrowser()
	command := exec.CommandContext(browserCtx, r.ChromePath,
		"--headless=new",
		"--disable-gpu",
		"--disable-extensions",
		"--disable-background-networking",
		"--hide-scrollbars",
		"--no-first-run",
		"--no-default-browser-check",
		"--user-data-dir="+inspectionProfile,
		"--window-size=1440,1000",
		"--virtual-time-budget=8000",
		"--screenshot="+screenshotPath,
		"--dump-dom",
		targetURL,
	)
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil && stdout.Len() == 0 {
		if errors.Is(browserCtx.Err(), context.DeadlineExceeded) {
			return Result{FinalURL: targetURL}, errors.New("Chrome 执行超过 25 秒工具预算")
		}
		return Result{FinalURL: targetURL}, fmt.Errorf("chrome: %w: %s", err, truncate(stderr.String(), 400))
	}
	document := stdout.String()
	title := extractTitle(document)
	bodyText := extractText(document)
	if _, err := os.Stat(screenshotPath); err != nil {
		return Result{Title: title, FinalURL: targetURL, BodyText: bodyText}, fmt.Errorf("chrome did not create screenshot: %w", err)
	}
	return Result{Title: title, FinalURL: targetURL, BodyText: bodyText, Screenshot: "/artifacts/" + taskID + "/overview.png"}, nil
}

func (r *Runner) Evaluate(plan []domain.CheckSpec, result Result) ([]domain.CheckResult, bool) {
	bodyLower := strings.ToLower(result.BodyText)
	titleLower := strings.ToLower(result.Title)
	challengeText := titleLower + " " + bodyLower
	challengeNouns := []string{"验证码", "安全验证", "滑块", "captcha", "verify"}
	challengeActions := []string{"请完成", "完成验证", "拖动滑块", "继续访问", "登录后继续", "verify you are human", "captcha challenge"}
	challengeTitles := []string{"验证码中间页", "安全验证中", "captcha challenge", "verify you are human"}
	hasChallenge := containsAny(titleLower, challengeTitles) || (containsAny(challengeText, challengeNouns) && containsAny(challengeText, challengeActions))
	checks := make([]domain.CheckResult, 0, len(plan))
	needsHuman := false
	for _, spec := range plan {
		check := domain.CheckResult{CheckSpec: spec, Status: domain.CheckUnverified}
		switch spec.Kind {
		case "page_reachable":
			check.Status = domain.CheckPassed
			check.Observed = "页面导航和 DOM 加载成功"
		case "title_present":
			if strings.TrimSpace(result.Title) != "" {
				check.Status = domain.CheckPassed
				check.Observed = "页面标题：" + truncate(result.Title, 160)
			} else {
				check.Status = domain.CheckFailed
				check.Observed = "页面标题为空"
			}
		case "challenge_absent":
			if hasChallenge {
				check.Status = domain.CheckNeedsHuman
				check.Observed = "检测到登录或安全验证提示"
				needsHuman = true
			} else {
				check.Status = domain.CheckPassed
				check.Observed = "未检测到已知验证提示"
			}
		case "contains_any":
			matched := matchingTerms(bodyLower, spec.Terms)
			if len(matched) > 0 {
				check.Status = domain.CheckPassed
				check.Observed = "找到相关文本：" + strings.Join(matched, "、")
			} else {
				check.Status = domain.CheckUnverified
				check.Observed = "没有找到指定文本；页面结构、登录状态或目标描述可能影响结果"
			}
		case "contains_all":
			matched := matchingTerms(bodyLower, spec.Terms)
			if len(matched) == len(nonEmptyTerms(spec.Terms)) {
				check.Status = domain.CheckPassed
				check.Observed = "全部预期文案均可见：" + strings.Join(matched, "、")
			} else {
				missing := missingTerms(bodyLower, spec.Terms)
				check.Status = domain.CheckFailed
				check.Observed = "缺少预期文案：" + strings.Join(missing, "、")
			}
		case "expected_live_status":
			expected := ""
			if len(spec.Terms) > 0 {
				expected = strings.ToLower(strings.TrimSpace(spec.Terms[0]))
			}
			live := containsAny(bodyLower, []string{"直播中", "正在直播", "live now"})
			offline := containsAny(bodyLower, []string{"未开播", "直播已结束", "已下播", "offline"})
			switch {
			case expected == "live" && live:
				check.Status, check.Observed = domain.CheckPassed, "页面显示正在直播"
			case expected == "offline" && offline:
				check.Status, check.Observed = domain.CheckPassed, "页面显示未开播或直播已结束"
			case expected == "live" && offline:
				check.Status, check.Observed = domain.CheckFailed, "预期正在直播，但页面显示未开播或已结束"
			case expected == "offline" && live:
				check.Status, check.Observed = domain.CheckFailed, "预期未开播，但页面显示正在直播"
			default:
				check.Status, check.Observed = domain.CheckUnverified, "页面中没有可确认直播状态的文本"
			}
		case "live_status":
			matched := matchingTerms(bodyLower, []string{"直播中", "正在直播", "未开播", "直播已结束", "live"})
			if len(matched) > 0 {
				check.Status = domain.CheckPassed
				check.Observed = "识别到状态文本：" + strings.Join(matched, "、")
			} else {
				check.Status = domain.CheckUnverified
				check.Observed = "未找到可确认直播状态的文本"
			}
		case "activity_entry":
			matched := matchingTerms(bodyLower, []string{"活动入口已开启", "活动入口：已开启", "进入活动"})
			if len(matched) > 0 {
				check.Status = domain.CheckPassed
				check.Observed = "找到已启用的活动入口：" + strings.Join(matched, "、")
			} else {
				check.Status = domain.CheckUnverified
				check.Observed = "浏览器可见文本无法确认活动入口已启用"
			}
		}
		if result.Screenshot != "" {
			check.Evidence = []string{result.Screenshot}
		}
		checks = append(checks, check)
	}
	return checks, needsHuman
}

func (r *Runner) validateURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return errors.New("仅支持有效的 http/https URL")
	}
	host := strings.ToLower(parsed.Hostname())
	for allowed := range r.AllowedHosts {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return nil
		}
	}
	return fmt.Errorf("目标域名 %q 不在允许列表中", host)
}

func containsAny(input string, terms []string) bool { return len(matchingTerms(input, terms)) > 0 }

func matchingTerms(input string, terms []string) []string {
	var matched []string
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term != "" && strings.Contains(input, strings.ToLower(term)) {
			matched = append(matched, term)
		}
	}
	return matched
}

func nonEmptyTerms(terms []string) []string {
	result := make([]string, 0, len(terms))
	for _, term := range terms {
		if term = strings.TrimSpace(term); term != "" {
			result = append(result, term)
		}
	}
	return result
}

func missingTerms(input string, terms []string) []string {
	var missing []string
	for _, term := range nonEmptyTerms(terms) {
		if !strings.Contains(input, strings.ToLower(term)) {
			missing = append(missing, term)
		}
	}
	return missing
}

func truncate(input string, max int) string {
	runes := []rune(strings.TrimSpace(input))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max]) + "…"
}

func extractTitle(document string) string {
	match := titlePattern.FindStringSubmatch(document)
	if len(match) != 2 {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(match[1]))
}

func extractText(document string) string {
	withoutScripts := scriptPattern.ReplaceAllString(document, " ")
	withoutTags := tagPattern.ReplaceAllString(withoutScripts, " ")
	return spacePattern.ReplaceAllString(strings.TrimSpace(html.UnescapeString(withoutTags)), " ")
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(input []byte) (int, error) {
	originalLength := len(input)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(input) > remaining {
			input = input[:remaining]
		}
		_, _ = b.Buffer.Write(input)
	}
	return originalLength, nil
}
