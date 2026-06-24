package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type FileCache map[string]string

func loadCache(dir string) FileCache {
	cachePath := filepath.Join(dir, cacheFileName)
	cache := make(FileCache)
	data, err := os.ReadFile(cachePath)
	if err == nil {
		json.Unmarshal(data, &cache)
	}
	return cache
}
func saveCache(dir string, cache FileCache) {
	cachePath := filepath.Join(dir, cacheFileName)
	data, _ := json.MarshalIndent(cache, "", "  ")
	os.WriteFile(cachePath, data, 0644)
}

const historyFileName = ".autodev_history.json"

// Încarcă istoricul conversației din fișier (dacă există)
func loadHistory() []ChatMessage {
	var history []ChatMessage
	data, err := os.ReadFile(historyFileName)
	if err == nil {
		json.Unmarshal(data, &history)
	}
	return history
}

// Salvează istoricul curent în fișier
func saveHistory(history []ChatMessage) {
	data, err := json.MarshalIndent(history, "", "  ")
	if err == nil {
		os.WriteFile(historyFileName, data, 0644)
	}
}
