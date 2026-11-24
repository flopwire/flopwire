package main

import (
	"encoding/json"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/antigravity"
)

func decodeHookInput(raw []byte, antigravityEvent string, in *hookInput) bool {
	if antigravityEvent == "" {
		if json.Unmarshal(raw, in) != nil {
			return false
		}
		// The Antigravity adapter requires its own command and payload.
		if in.Harness == string(transcript.AgentAntigravity) {
			in.Harness = ""
		}
		return true
	}

	var payload struct {
		ConversationID string `json:"conversationId"`
		TranscriptPath string `json:"transcriptPath"`
		FullyIdle      bool   `json:"fullyIdle"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return false
	}
	id, err := uuid.Parse(payload.ConversationID)
	if err != nil || id.String() != payload.ConversationID {
		return false
	}
	if p := payload.TranscriptPath; p != "" {
		// agy 1.5.16 hooks name the full trajectory. Only its canonical
		// sibling is indexed, matching background discovery.
		if filepath.Base(p) == "transcript_full.jsonl" {
			p = filepath.Join(filepath.Dir(p), "transcript.jsonl")
		}
		if !filepath.IsAbs(p) || antigravity.SessionID(p) != payload.ConversationID {
			return false
		}
		payload.TranscriptPath = p
	}

	switch antigravityEvent {
	case evSessionStart:
		in.Event = evSessionStart
	case "PreInvocation":
		in.Event = evUserPromptSubmit
	case "Stop":
		// Stop can fire while background work is still running. It
		// never means the Antigravity process closed the conversation.
		if payload.FullyIdle {
			in.Event = "Stop"
		}
	default:
		return false
	}
	in.SessionID = payload.ConversationID
	in.TranscriptPath = payload.TranscriptPath
	in.Harness = string(transcript.AgentAntigravity)
	return true
}

type antigravitySystemMessage struct {
	Text string `json:"systemMessage"`
}

type antigravityStep struct {
	SystemMessage antigravitySystemMessage `json:"systemMessage"`
}

type antigravityOutput struct {
	InjectSteps []antigravityStep `json:"injectSteps"`
}

func antigravityHookOutput(text string) antigravityOutput {
	return antigravityOutput{InjectSteps: []antigravityStep{{SystemMessage: antigravitySystemMessage{
		Text: text,
	}}}}
}
