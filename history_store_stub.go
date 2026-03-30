//go:build !windows

package main

import (
	"errors"
	"time"
)

type historyStore struct{}

func openHistoryStore(path string) (*historyStore, error) {
	return nil, errors.New("当前构建未启用 Windows SQLite 持久化")
}

func (s *historyStore) Close() error {
	return nil
}

func (s *historyStore) Insert(key string, cfg ServiceConfig, entry HistoryEntry) error {
	return nil
}

func (s *historyStore) LoadWindow(keys []string, since time.Time) (map[string][]HistoryEntry, error) {
	return map[string][]HistoryEntry{}, nil
}

func (s *historyStore) MaybeCleanup(now time.Time) error {
	return nil
}
