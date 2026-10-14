package httpapi

import (
	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/content"
)

// itemsResponse is the catalog list envelope (spec 6.12): {"items": [...]}, no pagination.
type itemsResponse[T any] struct {
	Items []T `json:"items"`
}

func newItems[T any](items []T) itemsResponse[T] {
	if items == nil {
		items = []T{}
	}
	return itemsResponse[T]{Items: items}
}

type topicResponse struct {
	ID            uuid.UUID `json:"id"`
	Code          string    `json:"code"`
	Name          string    `json:"name"`
	NameJA        string    `json:"name_ja"`
	Description   string    `json:"description"`
	IconKey       *string   `json:"icon_key"`
	ScenarioCount int       `json:"scenario_count"`
}

type vocabHintResponse struct {
	TermJA  string `json:"term_ja"`
	Reading string `json:"reading"`
	Meaning string `json:"meaning"`
}

type visualPromptResponse struct {
	Headline         string   `json:"headline"`
	Body             string   `json:"body"`
	StarterPhrasesJA []string `json:"starter_phrases_ja"`
	ImageKey         *string  `json:"image_key"`
}

type scenarioResponse struct {
	ID                  uuid.UUID            `json:"id"`
	TopicID             uuid.UUID            `json:"topic_id"`
	Code                string               `json:"code"`
	Name                string               `json:"name"`
	NameJA              string               `json:"name_ja"`
	Description         string               `json:"description"`
	VersionID           uuid.UUID            `json:"version_id"`
	Version             int                  `json:"version"`
	DifficultyLevel     int                  `json:"difficulty_level"`
	DifficultyBand      string               `json:"difficulty_band"`
	JLPTLabel           string               `json:"jlpt_label"`
	RecommendedLevelMin float64              `json:"recommended_level_min"`
	RecommendedLevelMax float64              `json:"recommended_level_max"`
	Situation           string               `json:"situation"`
	AIRole              string               `json:"ai_role"`
	UserRole            string               `json:"user_role"`
	Goal                string               `json:"goal"`
	Tasks               []string             `json:"tasks"`
	VocabHints          []vocabHintResponse  `json:"vocab_hints"`
	GrammarTargets      []string             `json:"grammar_targets"`
	Formality           content.Formality    `json:"formality"`
	VisualPrompt        visualPromptResponse `json:"visual_prompt"`
}

// characterResponse is the public character: nothing about the voice vendor or the persona.
type characterResponse struct {
	ID        uuid.UUID      `json:"id"`
	Code      string         `json:"code"`
	Name      string         `json:"name"`
	AvatarKey *string        `json:"avatar_key"`
	Gender    content.Gender `json:"gender"`
	Accent    content.Accent `json:"accent"`
}

func newTopicResponse(v content.TopicView) topicResponse {
	return topicResponse(v)
}

func newVisualPromptResponse(v content.VisualPrompt) visualPromptResponse {
	phrases := v.StarterPhrasesJA
	if phrases == nil {
		phrases = []string{}
	}
	return visualPromptResponse{Headline: v.Headline, Body: v.Body, StarterPhrasesJA: phrases, ImageKey: v.ImageKey}
}

func newScenarioResponse(v content.ScenarioView) scenarioResponse {
	hints := make([]vocabHintResponse, len(v.VocabHints))
	for i, h := range v.VocabHints {
		hints[i] = vocabHintResponse{TermJA: h.TermJA, Reading: h.Reading, Meaning: h.Meaning}
	}
	return scenarioResponse{
		ID: v.ID, TopicID: v.TopicID, Code: v.Code, Name: v.Name, NameJA: v.NameJA, Description: v.Description,
		VersionID: v.VersionID, Version: v.Version, DifficultyLevel: v.DifficultyLevel, DifficultyBand: v.DifficultyBand, JLPTLabel: v.JLPTLabel,
		RecommendedLevelMin: v.RecommendedLevelMin, RecommendedLevelMax: v.RecommendedLevelMax,
		Situation: v.Situation, AIRole: v.AIRole, UserRole: v.UserRole, Goal: v.Goal,
		Tasks: v.Tasks, VocabHints: hints, GrammarTargets: v.GrammarTargets, Formality: v.Formality,
		VisualPrompt: newVisualPromptResponse(v.VisualPrompt),
	}
}

func newCharacterResponse(v content.CharacterView) characterResponse {
	return characterResponse(v)
}
