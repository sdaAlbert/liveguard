package monitor

import (
	"strings"
	"time"
)

const presenceMissingSamplesRequired = 3

type presenceTransition struct {
	Term        string
	Kind        string
	MatchedText string
	LastSeenAt  time.Time
}

type presenceState struct {
	Initialized bool
	Present     bool
	Missing     int
	LastSeenAt  time.Time
	LastSeen    string
}

// OCRPresenceTracker turns repeated successful OCR samples into explicit
// visible-text transitions for goal terms. It tracks screen visibility only;
// absence from OCR is not proof that an item is unavailable or delisted.
type OCRPresenceTracker struct {
	goalVersion uint64
	terms       []string
	states      map[string]presenceState
}

func (t *OCRPresenceTracker) Configure(goalVersion uint64, terms []string) {
	normalized := make([]string, 0, min(3, len(terms)))
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term != "" && runeCount(term) <= 48 && !hasExactString(normalized, term) {
			normalized = append(normalized, term)
		}
		if len(normalized) == 3 {
			break
		}
	}
	if t.goalVersion == goalVersion && equalStrings(t.terms, normalized) {
		return
	}
	t.goalVersion = goalVersion
	t.terms = normalized
	t.states = make(map[string]presenceState, len(normalized))
}

// Preview is transactional with downstream signal admission: the tracker
// advances only after the resulting signal has been accepted.
func (t *OCRPresenceTracker) Preview(text string, observedAt time.Time) ([]presenceTransition, func()) {
	if t == nil || len(t.terms) == 0 {
		return nil, func() {}
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	next := OCRPresenceTracker{goalVersion: t.goalVersion, terms: append([]string(nil), t.terms...), states: make(map[string]presenceState, len(t.states))}
	for term, state := range t.states {
		next.states[term] = state
	}
	var transitions []presenceTransition
	for _, term := range next.terms {
		matched := matchingPresenceLine(text, term)
		state := next.states[term]
		if !state.Initialized {
			state.Initialized = true
			state.Present = matched != ""
			if matched != "" {
				state.LastSeenAt, state.LastSeen = observedAt.UTC(), matched
			}
			next.states[term] = state
			continue
		}
		if matched != "" {
			if !state.Present {
				transitions = append(transitions, presenceTransition{Term: term, Kind: "appeared", MatchedText: matched})
			}
			state.Present, state.Missing = true, 0
			state.LastSeenAt, state.LastSeen = observedAt.UTC(), matched
			next.states[term] = state
			continue
		}
		if state.Present {
			state.Missing++
			if state.Missing >= presenceMissingSamplesRequired {
				transitions = append(transitions, presenceTransition{Term: term, Kind: "disappeared", MatchedText: state.LastSeen, LastSeenAt: state.LastSeenAt})
				state.Present, state.Missing = false, 0
			}
		}
		next.states[term] = state
	}
	return transitions, func() { *t = next }
}

func matchingPresenceLine(text, term string) string {
	needle := normalizeOCRLine(term)
	if needle == "" {
		return ""
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && strings.Contains(normalizeOCRLine(line), needle) {
			return line
		}
	}
	return ""
}

func normalizePresenceTerms(proposed []string, goal string) []string {
	if !goalRequestsPresenceTracking(goal) {
		return nil
	}
	normalizedGoal := normalizeOCRLine(goal)
	terms := make([]string, 0, min(3, len(proposed)))
	seen := map[string]bool{}
	for _, term := range proposed {
		term = strings.TrimSpace(term)
		if runeCount(term) == 0 || runeCount(term) > 48 {
			continue
		}
		key := normalizeOCRLine(term)
		if key == "" || genericPresenceTerm(key) || seen[key] || !strings.Contains(normalizedGoal, key) {
			continue
		}
		seen[key] = true
		terms = append(terms, term)
		if len(terms) == 3 {
			break
		}
	}
	return terms
}

func presenceTermsFromGoal(goal string) []string {
	if !goalRequestsPresenceTracking(goal) {
		return nil
	}
	candidate := strings.ToLower(strings.TrimSpace(goal))
	for _, phrase := range []string{
		"请实时监控", "实时监控", "监控", "帮我盯着", "帮我关注", "我想关注", "我关注的", "提醒我", "通知我", "告诉我", "请提醒", "请通知",
		"每一条新消息", "每条新消息", "说出的", "说出", "主播", "直播间",
		"出现的时候", "消失的时候", "出现时", "消失时", "出现后", "消失后", "出现", "消失",
		"上架的时候", "下架的时候", "上架时", "下架时", "上架后", "下架后", "上架", "下架",
		"补货的时候", "售罄的时候", "有货的时候", "缺货的时候", "补货", "售罄", "有货", "缺货",
		"when", "whenever", "notify me", "alert me", "tell me", "appears", "appear", "disappears", "disappear", "listed", "delisted", "in stock", "out of stock",
		"商品", "产品", "货品", "这个", "那个", "这款", "目标", "时", "后", "和", "与", "或", "都", "也", "又", "的", "我", "当", "如果", "一旦",
	} {
		candidate = strings.ReplaceAll(candidate, phrase, " ")
	}
	candidate = strings.TrimSpace(strings.Join(strings.Fields(candidate), " "))
	candidate = strings.Trim(candidate, "，。！？；：、/\\-_")
	if candidate == "" || runeCount(candidate) > 48 || genericPresenceTerm(normalizeOCRLine(candidate)) {
		return nil
	}
	return normalizePresenceTerms([]string{candidate}, goal)
}

func genericPresenceTerm(term string) bool {
	switch term {
	case "某", "某个", "商品", "产品", "货品", "目标", "item", "product", "target":
		return true
	default:
		return false
	}
}

func goalRequestsPresenceTracking(goal string) bool {
	value := strings.ToLower(goal)
	// Require an explicit disappearance/absence concept so generic goals such
	// as "每条新消息出现时提醒我" do not start tracking every OCR line.
	cues := []string{"消失", "下架", "售罄", "缺货", "不见", "disappear", "delisted", "out of stock", "no longer visible"}
	for _, cue := range cues {
		if strings.Contains(value, cue) {
			return true
		}
	}
	return false
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func hasExactString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
