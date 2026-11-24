// Package state persists ~/.cache/gemini-chat/state.json: the round-robin
// rotation pointer (last_authuser) and per-account chat continuation data
// (metadata, cid, rid, rcid, updated). The file is shared with the Python
// gemini-chat, so the schema must stay identical.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ChatEntry is one saved conversation.
type ChatEntry struct {
	AuthUser int    `json:"authuser"`
	Metadata []any  `json:"metadata"`
	CID      string `json:"cid"`
	RID      string `json:"rid"`
	RCID     string `json:"rcid"`
	Updated  string `json:"updated"`
}

// State mirrors the Python state.json schema.
type State struct {
	Chats        map[string]*ChatEntry `json:"chats"`
	LastAuthUser string                `json:"last_authuser"`
}

func statePath() string {
	if v := os.Getenv("GEMINI_CHAT_STATE"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "gemini-chat", "state.json")
}

// Path returns the resolved state file path (for --list output).
func Path() string { return statePath() }

// Load reads state.json; a missing or corrupt file yields an empty state,
// matching the Python load_state.
func Load() *State {
	s := &State{Chats: map[string]*ChatEntry{}}
	raw, err := os.ReadFile(statePath())
	if err != nil {
		return s
	}
	var data struct {
		Chats        map[string]*ChatEntry `json:"chats"`
		LastAuthUser string                `json:"last_authuser"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return s
	}
	if data.Chats != nil {
		s.Chats = data.Chats
	}
	s.LastAuthUser = data.LastAuthUser
	return s
}

// Save writes state.json atomically with mode 0600, like the Python
// save_state (tempfile + os.replace).
func (s *State) Save() error {
	path := statePath()
	if path == "" {
		return fmt.Errorf("cannot resolve state path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// SaveChat records the chat state after a successful send and updates
// last_authuser, then persists.
func (s *State) SaveChat(name string, authUser int, metadata []any, cid, rid, rcid string) error {
	cidStr, ridStr, rcidStr := "", "", ""
	if len(metadata) > 0 {
		if v, ok := metadata[0].(string); ok {
			cidStr = v
		}
		if len(metadata) > 1 {
			if v, ok := metadata[1].(string); ok {
				ridStr = v
			}
		}
		if len(metadata) > 2 {
			if v, ok := metadata[2].(string); ok {
				rcidStr = v
			}
		}
	}
	s.Chats[name] = &ChatEntry{
		AuthUser: authUser,
		Metadata: metadata,
		CID:      cidStr,
		RID:      ridStr,
		RCID:     rcidStr,
		Updated:  time.Now().UTC().Format("2006-01-02T15:04:05+00:00"),
	}
	s.LastAuthUser = fmt.Sprint(authUser)
	return s.Save()
}
