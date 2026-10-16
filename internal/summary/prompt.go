package summary

import "strings"

// SummaryPromptData is the data of the SUMMARY templates (spec 9.9.6).
type SummaryPromptData struct { //nolint:revive // name fixed by the spec (9.9.6)
	MaxChars      int    // SUMMARY_MAX_CHARS (default 300)
	TopicName     string // topics.name
	ScenarioName  string // scenarios.name
	UserTurnCount int    // payload.user_turn_count
	Transcript    string // BuildTranscript output
}

// PromptFixtures are the data values the publish validator renders the SUMMARY templates against
// (spec 6.10.1): minimal (one transcript line) and maximal (30,000-character transcript).
func PromptFixtures() []any {
	minimal := SummaryPromptData{
		MaxChars: DefaultMaxChars, TopicName: "日常生活", ScenarioName: "自己紹介", UserTurnCount: 1,
		Transcript: "ユーザー: はじめまして。",
	}
	const unit = "ユーザー: 週末は友達と京都へ行きました。\nAI: いいですね。どこを見ましたか？\n"
	n := TranscriptMaxChars/len([]rune(unit)) + 1
	maximal := SummaryPromptData{
		MaxChars: DefaultMaxChars, TopicName: strings.Repeat("旅行", 20), ScenarioName: strings.Repeat("ホテルの予約", 10),
		UserTurnCount: n, Transcript: string([]rune(strings.Repeat(unit, n))[:TranscriptMaxChars]),
	}
	return []any{minimal, maximal}
}
