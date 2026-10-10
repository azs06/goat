package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/azs06/goat/internal/provider"
)

const (
	sessionPath         = stateDirName + "/session.json"
	previousSessionPath = stateDirName + "/session.prev.json"
	sessionVersion      = 1
)

// savedSession is what survives a restart: the model, the effort, and where the
// conversation continues from.
type savedSession struct {
	Version            int                   `json:"version"`
	ConversationID     string                `json:"conversation_id,omitempty"`
	Model              string                `json:"model"`
	Effort             string                `json:"effort,omitempty"`
	Conversation       provider.Conversation `json:"conversation"`
	PendingToolResults []provider.ToolResult `json:"pending_tool_results,omitempty"`
	// PromptFingerprint identifies the system prompt and tools the conversation was
	// built with. Anthropic only accepts replayed thinking blocks under the same ones.
	PromptFingerprint string `json:"prompt_fingerprint,omitempty"`
	SavedAt           string `json:"saved_at"`
}

func promptFingerprint() string {
	tools, _ := json.Marshal(agentTools)
	sum := sha256.Sum256(append([]byte(SystemPrompt+"\x00"), tools...))
	return hex.EncodeToString(sum[:])
}

func sessionFromConfig(cfg *config) savedSession {
	return savedSession{
		Version:            sessionVersion,
		ConversationID:     activeConversationID,
		Model:              cfg.model.String(),
		Effort:             string(cfg.reasoningEffort),
		Conversation:       cfg.conversation.conversation,
		PendingToolResults: cfg.conversation.pendingToolResults,
		PromptFingerprint:  cfg.conversationFingerprint(),
		SavedAt:            time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// sessionSaver is a variable so tests can observe saves.
var sessionSaver = writeSessionFile

func saveSession(cfg *config) {
	if err := sessionSaver(sessionPath, sessionFromConfig(cfg)); err != nil {
		warnf("failed to save the session: %v", err)
	}
}

func writeSessionFile(path string, session savedSession) error {
	resolvedPath, _, err := resolveStatePath(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolvedPath), 0755); err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}

	// Write a temporary file and rename it so a crash never leaves a truncated session.
	tempFile, err := os.CreateTemp(filepath.Dir(resolvedPath), "session-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tempFile.Name())
	if err := tempFile.Chmod(0600); err != nil {
		tempFile.Close()
		return err
	}
	if _, err := tempFile.Write(append(encoded, '\n')); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Sync(); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	return os.Rename(tempFile.Name(), resolvedPath)
}

// loadSessionFile returns nil when there is no session. An unreadable file is set
// aside with a warning so it cannot block startup.
func loadSessionFile(path string) (*savedSession, error) {
	resolvedPath, _, err := resolveStatePath(path)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}

	var session savedSession
	decodeErr := json.Unmarshal(data, &session)
	if decodeErr == nil && session.Version != sessionVersion {
		decodeErr = fmt.Errorf("unsupported session version %d", session.Version)
	}
	if decodeErr != nil {
		corruptPath := fmt.Sprintf("%s.corrupt-%d", resolvedPath, time.Now().UnixNano())
		if renameErr := os.Rename(resolvedPath, corruptPath); renameErr != nil {
			return nil, fmt.Errorf("read %s: %w", path, decodeErr)
		}
		warnf("%s was unreadable (%v); moved it to %s", path, decodeErr, filepath.Base(corruptPath))
		return nil, nil
	}
	return &session, nil
}

// archiveSession keeps the current conversation in session.prev.json before it is
// replaced, so /resume can bring it back.
func archiveSession(cfg *config) {
	if cfg.conversation.conversation.IsEmpty() && len(cfg.conversation.pendingToolResults) == 0 {
		return
	}
	if err := sessionSaver(previousSessionPath, sessionFromConfig(cfg)); err != nil {
		warnf("failed to keep the previous session: %v", err)
	}
}

// replaceConversation is the one way to start a new conversation: it keeps the old one
// for /resume and saves the new state. Callers record the switch in the history log.
func replaceConversation(cfg *config, next conversationState) {
	archiveSession(cfg)
	startConversation(cfg, next)
}

// startConversation makes next the current conversation under a new ID and saves it.
func startConversation(cfg *config, next conversationState) {
	activeConversationID = newConversationID()
	cfg.conversation = next
	cfg.staleConversation = false
	cfg.promptFingerprint = promptFingerprint()
	saveSession(cfg)
}

// adoptSavedConversation continues a saved conversation, including its history ID.
func adoptSavedConversation(cfg *config, saved *savedSession) {
	activeConversationID = saved.ConversationID
	if activeConversationID == "" && saved.Conversation.IsEmpty() {
		activeConversationID = newConversationID()
	}
	cfg.conversation = conversationState{conversation: saved.Conversation, pendingToolResults: saved.PendingToolResults}
	cfg.staleConversation = conversationIsStale(saved)
	cfg.promptFingerprint = saved.PromptFingerprint
	if saved.Conversation.IsEmpty() {
		// Nothing was built yet, so whatever comes next is built with the current prompt.
		cfg.promptFingerprint = promptFingerprint()
	}
}

// conversationFingerprint is the prompt fingerprint the conversation was built with,
// so saving a stale conversation never makes it look current.
func (c *config) conversationFingerprint() string {
	if c.promptFingerprint == "" {
		return promptFingerprint()
	}
	return c.promptFingerprint
}

// conversationIsStale reports a saved transcript built with a different system prompt
// or tool set. Only replayed transcripts care: OpenAI resends both every request.
func conversationIsStale(saved *savedSession) bool {
	return len(saved.Conversation.Messages) > 0 && saved.PromptFingerprint != "" && saved.PromptFingerprint != promptFingerprint()
}
