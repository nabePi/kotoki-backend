package evaluation

import (
	"encoding/json"
	"strings"

	"github.com/nabePi/kotoki-backend/internal/content"
)

// EvaluationPromptData is the data of the EVALUATION templates (spec 10.6.2).
type EvaluationPromptData struct { //nolint:revive // name fixed by the spec (10.6.2)
	RubricVersion      string // RubricVersionOf(pv), e.g. "rubric-v1"
	FindingTypes       string // code: one line per type (spec 10.1)
	PatternKeyExamples string // code: comma-separated examples
	TopicTitle         string // topics.name
	ScenarioTitle      string // scenarios.name
	DifficultyLevel    int    // scenario_versions.difficulty_level (1–5)
	LearnerRole        string // scenario_versions.user_role
	AIRole             string // scenario_versions.ai_role
	CharacterName      string // sessions.config_snapshot->'character'->>'name'
	Objectives         []string
	ScenarioContext    string // scenario_versions.situation
	ExpectedFormality  string // phrase of the session formality (S-17)
	EvaluationFocus    []string
	UserTurnCount      int
	UserSpeechSeconds  int
	ActiveSeconds      int
	Transcript         string
}

// PromptInput is what BuildPromptData derives EvaluationPromptData from.
type PromptInput struct {
	TopicName, ScenarioName, CharacterName string
	SV                                     content.ScenarioVersion
	SessionAdaptive                        json.RawMessage // sessions.adaptive_difficulty
	RubricVersion                          string
	Stats                                  Stats
	Transcript                             string
}

// BuildPromptData assembles the template data (spec 10.6.2).
func BuildPromptData(in PromptInput) EvaluationPromptData {
	sv := in.SV
	objectives := append([]string{sv.Goal}, sv.Tasks...)
	if len(sv.GrammarTargets) > 0 {
		objectives = append(objectives, "Use: "+strings.Join(sv.GrammarTargets, ", "))
	}
	return EvaluationPromptData{
		RubricVersion:      in.RubricVersion,
		FindingTypes:       FindingTypesText(),
		PatternKeyExamples: PatternKeyExamplesText(),
		TopicTitle:         in.TopicName,
		ScenarioTitle:      in.ScenarioName,
		DifficultyLevel:    sv.DifficultyLevel,
		LearnerRole:        sv.UserRole,
		AIRole:             sv.AIRole,
		CharacterName:      in.CharacterName,
		Objectives:         objectives,
		ScenarioContext:    sv.Situation,
		ExpectedFormality:  ExpectedFormality(in.SessionAdaptive, sv),
		EvaluationFocus:    sv.EvaluationFocus,
		UserTurnCount:      in.Stats.UserTurns,
		UserSpeechSeconds:  in.Stats.UserSpeechSeconds,
		ActiveSeconds:      in.Stats.ActiveSeconds,
		Transcript:         in.Transcript,
	}
}

const (
	phraseCasual     = "casual (plain form, タメ口)"
	phrasePolite     = "polite (です・ます)"
	phraseKeigoLight = "polite (です・ます); fixed keigo phrases of the role are expected, and any correct keigo is also acceptable"
	phraseKeigo      = "polite/keigo (です・ます, 尊敬語・謙譲語 where appropriate)"
)

// formalityPhrase maps a formality name (session value or scenario base value, D-014) to the
// phrase of the template; false for an unknown name.
func formalityPhrase(name string) (string, bool) {
	switch name {
	case "CASUAL":
		return phraseCasual, true
	case "POLITE":
		return phrasePolite, true
	case "KEIGO_LIGHT":
		return phraseKeigoLight, true
	case "KEIGO":
		return phraseKeigo, true
	}
	return "", false
}

// ExpectedFormality is the expected register phrase (S-17): the session formality
// (sessions.adaptive_difficulty.formality) when present and known, otherwise the scenario base
// formality. Only the formality key is parsed, so this package does not import learner.
func ExpectedFormality(adaptive json.RawMessage, sv content.ScenarioVersion) string {
	var a struct {
		Formality string `json:"formality"`
	}
	if len(adaptive) > 0 && json.Unmarshal(adaptive, &a) == nil {
		if p, ok := formalityPhrase(a.Formality); ok {
			return p
		}
	}
	if p, ok := formalityPhrase(string(content.ScenarioFormality(sv))); ok {
		return p
	}
	return phrasePolite
}

const strengthLine = "STRENGTH -> any of GRAMMAR, VOCABULARY, FLUENCY, NATURALNESS, TASK_ACHIEVEMENT; " +
	"severity must be INFO; turn_seq and original optional; corrected must be null"

// FindingTypesText is the FindingTypes template value: one line per type, built from SpecOf.
func FindingTypesText() string {
	lines := make([]string, 0, len(AllFindingTypes))
	for _, t := range AllFindingTypes {
		if t == FindingStrength {
			lines = append(lines, strengthLine)
			continue
		}
		s := specs[t]
		fields := []struct {
			name string
			req  Req
		}{{"turn_seq", s.TurnSeq}, {"original", s.Original}, {"corrected", s.Corrected}}
		var parts []string
		for _, want := range []struct {
			req    Req
			suffix string
		}{{ReqRequired, " required"}, {ReqOptional, " optional"}, {ReqNull, " must be null"}} {
			var names []string
			for _, f := range fields {
				if f.req == want.req {
					names = append(names, f.name)
				}
			}
			if len(names) > 0 {
				parts = append(parts, strings.Join(names, ", ")+want.suffix)
			}
		}
		lines = append(lines, string(t)+" -> "+string(s.Dimension)+"; "+strings.Join(parts, "; "))
	}
	return strings.Join(lines, "\n")
}

var patternKeyExamples = []string{
	"particle.de_ni_location", "particle.wa_ga", "particle.wo_omission", "conjugation.i_adjective_past",
	"conjugation.te_form", "aspect.teiru", "politeness.plain_in_formal", "politeness.keigo_misuse",
	"vocab.english_substitution", "vocab.wrong_counter", "structure.missing_predicate",
	"fluency.one_word_answers", "task.no_follow_up_question",
}

// PatternKeyExamplesText is the PatternKeyExamples template value.
func PatternKeyExamplesText() string { return strings.Join(patternKeyExamples, ", ") }

func repeatRunes(unit string, n int) string {
	r := []rune(strings.Repeat(unit, n/len([]rune(unit))+1))
	return string(r[:n])
}

// PromptFixtures are the data values the publish validator renders the EVALUATION templates
// against (spec 6.10.1): minimal (empty slices, 3-turn transcript) and maximal (60,000-character
// transcript, every slice filled).
func PromptFixtures() []any {
	minimal := BuildPromptData(PromptInput{
		TopicName: "Daily life", ScenarioName: "A short chat", CharacterName: "葵",
		SV: content.ScenarioVersion{
			DifficultyLevel: 1, Situation: "A short chat.", AIRole: "A friend", UserRole: "A learner",
			Goal: "Chat for a few minutes", Formality: content.FormalityPolite,
		},
		RubricVersion: "rubric-v1",
		Stats:         Stats{UserTurns: 3, UserSpeechSeconds: 20, ActiveSeconds: 60},
		Transcript: "[T0] AI: こんにちは！\n" +
			"[T1] USER (2.0 s, 4.0 chars/s): こんにちは、元気です。\n" +
			"[T1] AI: それはよかった！\n" +
			"[T2] USER (2.0 s, 4.0 chars/s): はい、ありがとう。\n" +
			"[T2] AI: 今日は何をしましたか？\n" +
			"[T3] USER (2.0 s, 4.0 chars/s): 勉強しました。",
	})
	minimal.Objectives = nil
	minimal.EvaluationFocus = nil

	maximal := BuildPromptData(PromptInput{
		TopicName: "Travel and holidays", ScenarioName: "Booking a hotel for a long trip",
		CharacterName: "葵",
		SV: content.ScenarioVersion{
			DifficultyLevel: 5, Situation: repeatRunes("A hotel front desk situation. ", 2000),
			AIRole: "Hotel clerk", UserRole: "Guest", Goal: "Book a room and ask about facilities",
			Tasks:           []string{"Ask about prices", "Ask about breakfast", "Confirm the check-out time"},
			GrammarTargets:  []string{"〜ていただけませんか", "〜ので", "〜たら"},
			EvaluationFocus: []string{"GRAMMAR", "VOCABULARY", "NATURALNESS"},
			Formality:       content.FormalityKeigo,
		},
		SessionAdaptive: json.RawMessage(`{"formality":"KEIGO_LIGHT"}`),
		RubricVersion:   "rubric-v99",
		Stats:           Stats{UserTurns: 400, UserSpeechSeconds: 3600, ActiveSeconds: 7200},
		Transcript:      repeatRunes("[T1] USER (4.2 s, 5.0 chars/s): えっと、今日は会社に働いていました。\n", 60000),
	})
	return []any{minimal, maximal}
}

// ExampleOutput is a valid output of the v1 rubric schema (spec 10.7.1); it must pass the
// published output_schema (guarantees the schema stays compatible with the EvalOutput parser).
const ExampleOutput = `{
  "scores": {
    "GRAMMAR": {"score": 52, "confidence": 0.8, "comment": "Kalimat sederhanamu sudah jelas, tetapi partikel lokasi dan bentuk lampau kata sifat masih sering keliru."},
    "VOCABULARY": {"score": 55, "confidence": 0.7, "comment": "Kosakata sehari-hari cukup, tetapi masih terbatas untuk topik pekerjaan."},
    "FLUENCY": {"score": 45, "confidence": 0.75, "comment": "Jawabanmu sering sangat pendek; coba tambahkan alasan atau contoh."},
    "NATURALNESS": {"score": 50, "confidence": 0.7, "comment": "Bentuk です・ます sudah konsisten, namun beberapa ungkapan terdengar kaku."},
    "TASK_ACHIEVEMENT": {"score": 60, "confidence": 0.8, "comment": "Kamu menjawab semua pertanyaan, tetapi belum bertanya balik."}
  },
  "signals": {
    "LISTENING_RESPONSE": {"score": 62, "confidence": 0.7},
    "INTERACTION_COMPLEXITY": {"score": 38, "confidence": 0.6}
  },
  "summary": "Kamu bisa mempertahankan percakapan tentang pekerjaan dengan kalimat sederhana. Fokus berikutnya: partikel で untuk tempat aktivitas dan bentuk lampau kata sifat.",
  "strengths": ["Penggunaan です・ます konsisten sepanjang percakapan."],
  "focus_areas": ["Partikel で vs に untuk tempat", "Bentuk lampau kata sifat-i"],
  "recommendations": [
    {"title": "Latihan partikel で", "detail": "Buat 5 kalimat tentang tempat kamu melakukan aktivitas sehari-hari.", "example_ja": "図書館で勉強します。"}
  ],
  "findings": [
    {"dimension": "GRAMMAR", "finding_type": "PARTICLE_ERROR", "severity": "MAJOR", "turn_seq": 1,
     "original": "会社に働いていました", "corrected": "会社で働いていました",
     "explanation": "Tempat berlangsungnya aktivitas ditandai dengan で, bukan に. Kesalahan ini muncul 2 kali.",
     "pattern_key": "particle.de_ni_location", "confidence": 0.95},
    {"dimension": "GRAMMAR", "finding_type": "CONJUGATION_ERROR", "severity": "MINOR", "turn_seq": 2,
     "original": "忙しいでした", "corrected": "忙しかったです",
     "explanation": "Bentuk lampau kata sifat-i adalah ～かったです, bukan ～いでした.",
     "pattern_key": "conjugation.i_adjective_past", "confidence": 0.97}
  ]
}`
