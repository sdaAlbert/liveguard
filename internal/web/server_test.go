package web

import "testing"

func TestValidateExpectedTexts(t *testing.T) {
	texts, err := validateExpectedTexts([]string{" 活动入口 ", "活动入口", "直播中"})
	if err != nil || len(texts) != 2 || texts[0] != "活动入口" {
		t.Fatalf("unexpected validation result %#v: %v", texts, err)
	}
	if _, err := validateExpectedTexts([]string{"1", "2", "3", "4", "5", "6"}); err == nil {
		t.Fatal("expected limit error")
	}
}
