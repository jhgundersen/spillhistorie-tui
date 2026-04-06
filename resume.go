package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type resumeState struct {
	AudioURL string  `json:"audio_url"`
	Title    string  `json:"title"`
	Series   string  `json:"series"`
	Duration float64 `json:"duration"`
	Pos      float64 `json:"pos"`
}

func resumePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "spillhistorie-tui")
	os.MkdirAll(dir, 0700)
	return filepath.Join(dir, "resume.json")
}

func loadResume() *resumeState {
	data, err := os.ReadFile(resumePath())
	if err != nil {
		return nil
	}
	var s resumeState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil
	}
	// Only resume if meaningful progress (>10s) and not nearly finished
	if s.Pos < 10 {
		return nil
	}
	if s.Duration > 0 && s.Pos > s.Duration-30 {
		return nil
	}
	return &s
}

func saveResume(s *resumeState) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	os.WriteFile(resumePath(), data, 0600)
}

func deleteResume() {
	os.Remove(resumePath())
}
