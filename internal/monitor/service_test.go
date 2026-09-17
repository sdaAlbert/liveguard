package monitor

import (
	"context"
	"testing"
	"time"
)

func TestPersonalMonitorsRunIndependentlyWithDifferentGoals(t *testing.T) {
	repository := NewMemoryRepository()
	service := NewService(repository, nil)
	service.SetReplayGap(time.Millisecond)
	coupon, err := service.Start(context.Background(), StartInput{TargetURL: "https://live.douyin.com/1001", Goal: "出现 50 元以上优惠券时提醒我"})
	if err != nil {
		t.Fatal(err)
	}
	game, err := service.Start(context.Background(), StartInput{TargetURL: "https://live.douyin.com/2002", Goal: "游戏进入决胜局或关键团战时提醒我"})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		coupon, _ = service.Get(context.Background(), coupon.ID)
		game, _ = service.Get(context.Background(), game.ID)
		if len(coupon.Events) == 6 && len(game.Events) == 6 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(coupon.Events) != 6 || len(game.Events) != 6 {
		t.Fatalf("signal replays did not finish: coupon=%d game=%d", len(coupon.Events), len(game.Events))
	}
	if coupon.Status != RunRunning || game.Status != RunRunning {
		t.Fatalf("personal monitors must remain active after the sample burst")
	}
	if len(coupon.Alerts) != 1 || coupon.Alerts[0].EventType != "coupon_drop" || len(game.Alerts) != 1 || game.Alerts[0].EventType != "game_moment" {
		t.Fatalf("goals should produce different alerts: coupon=%#v game=%#v", coupon.Alerts, game.Alerts)
	}

	statuses := map[string]int{}
	toolPaths := map[string]bool{}
	for _, investigation := range coupon.Investigations {
		statuses[investigation.Status]++
		path := ""
		for _, step := range investigation.Steps {
			path += step.Tool + ">"
		}
		toolPaths[path] = true
	}
	if statuses[InvestigationConfirmed] != 1 || statuses[InvestigationSuppressed] != 2 {
		t.Fatalf("unexpected decisions: %#v", statuses)
	}
	if len(toolPaths) < 3 {
		t.Fatalf("expected dynamic tool paths, got %#v", toolPaths)
	}
	service.StopAll()
}

func TestAmbiguousSignalWaitsForCrossSourceEvidence(t *testing.T) {
	repository := NewMemoryRepository()
	now := time.Now().UTC()
	run := &Run{ID: "monitor-test", Name: "test", Goal: "出现优惠券时提醒我", Status: RunRunning, StartedAt: now, UpdatedAt: now, Version: 1, Rooms: []Room{{ID: "room", Name: "room", Status: "listening"}}}
	if err := repository.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	service := NewService(repository, nil)
	if err := service.Ingest(context.Background(), run.ID, SignalEvent{RoomID: "room", RoomName: "room", Source: "asr", Text: "等一下给大家一波福利", Confidence: .78}); err != nil {
		t.Fatal(err)
	}
	current, _ := repository.Get(context.Background(), run.ID)
	if current.Investigations[0].Status != InvestigationWaiting || len(current.Alerts) != 0 {
		t.Fatalf("ambiguous signal must wait: %#v", current.Investigations[0])
	}
	if err := service.Ingest(context.Background(), run.ID, SignalEvent{RoomID: "room", RoomName: "room", Source: "ocr", Text: "限量优惠券 50元", Confidence: .81}); err != nil {
		t.Fatal(err)
	}
	current, _ = repository.Get(context.Background(), run.ID)
	if current.Investigations[0].Status != InvestigationConfirmed || len(current.Alerts) != 1 {
		t.Fatalf("cross-source evidence must confirm: %#v", current.Investigations[0])
	}
}
