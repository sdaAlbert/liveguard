package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"liveguard/internal/domain"
)

var ErrNotFound = errors.New("task not found")

type TaskRepository interface {
	RecoverInterrupted() error
	Create(*domain.Task) error
	CreateMany([]*domain.Task) error
	Update(string, func(*domain.Task) error) (*domain.Task, error)
	Get(string) (*domain.Task, error)
	List() []*domain.Task
}

type Store struct {
	mu    sync.RWMutex
	path  string
	tasks map[string]*domain.Task
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: path, tasks: make(map[string]*domain.Task)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 8*1024*1024)
	for scanner.Scan() {
		var task domain.Task
		if json.Unmarshal(scanner.Bytes(), &task) == nil {
			copy := task
			s.tasks[task.ID] = &copy
		}
	}
	return scanner.Err()
}

func (s *Store) RecoverInterrupted() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, task := range s.tasks {
		if task.Status == domain.StatusPlanning || task.Status == domain.StatusRunning {
			task.Status = domain.StatusQueued
			task.Error = ""
			task.AddEvent("recovery", "检测到未完成任务，已重新进入队列")
			if err := s.appendLocked(task); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) Create(task *domain.Task) error {
	return s.CreateMany([]*domain.Task{task})
}

func (s *Store) CreateMany(tasks []*domain.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, task := range tasks {
		if _, exists := s.tasks[task.ID]; exists {
			return errors.New("task already exists")
		}
	}
	for _, task := range tasks {
		copy := clone(task)
		if err := s.appendLocked(copy); err != nil {
			return err
		}
		s.tasks[task.ID] = copy
	}
	return nil
}

func (s *Store) Update(id string, mutate func(*domain.Task) error) (*domain.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.tasks[id]
	if !ok {
		return nil, ErrNotFound
	}
	next := clone(current)
	if err := mutate(next); err != nil {
		return nil, err
	}
	next.UpdatedAt = time.Now().UTC()
	next.Version++
	s.tasks[id] = next
	if err := s.appendLocked(next); err != nil {
		return nil, err
	}
	return clone(next), nil
}

func (s *Store) Get(id string) (*domain.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task, ok := s.tasks[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(task), nil
}

func (s *Store) List() []*domain.Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*domain.Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		result = append(result, clone(task))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}

func (s *Store) appendLocked(task *domain.Task) error {
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(data, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func clone(task *domain.Task) *domain.Task {
	data, _ := json.Marshal(task)
	var copy domain.Task
	_ = json.Unmarshal(data, &copy)
	return &copy
}
