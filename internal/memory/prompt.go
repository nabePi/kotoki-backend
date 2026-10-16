package memory

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// TranscriptMaxRunes is the longest transcript sent to the LLM (spec 11.5.1 step 6).
const TranscriptMaxRunes = 40000

// MaxExistingMemories is the number of existing memories shown to the LLM (step 7).
const MaxExistingMemories = 150

// minUserRunes is the length from which a USER turn counts for the skip rule (G4).
const minUserRunes = 5

// MemoryExtractionPromptData is the data of the MEMORY_EXTRACTION templates (spec 11.5.2).
type MemoryExtractionPromptData struct { //nolint:revive // name fixed by spec 11.5.2
	SessionDate      string               // sessions.ended_at, YYYY-MM-DD in Asia/Jakarta
	ScenarioTitle    string               // scenarios.name
	ExistingMemories []ExistingMemoryLine // empty when the user has none
	Transcript       string               // BuildTranscript output
}

// ExistingMemoryLine is one existing memory shown to the LLM so that it reuses keys.
type ExistingMemoryLine struct {
	Category string // PROFILE_FACT | PREFERENCE | LEARNING_PATTERN
	Key      string // memory_key
	Content  string // content (Japanese), newline -> space, '<' '>' -> fullwidth
}

// NewExistingMemoryLine builds a line with the content sanitised as the template requires.
func NewExistingMemoryLine(c Category, key, content string) ExistingMemoryLine {
	return ExistingMemoryLine{Category: string(c), Key: key, Content: sanitizeText(content)}
}

// TurnRow is one finished turn as the memory worker loads it (status COMPLETED or INTERRUPTED).
type TurnRow struct {
	ID                  uuid.UUID
	Seq                 int
	Status              string
	UserText            *string
	AssistantText       string
	AssistantSpokenText string
}

// Turn statuses that belong in the transcript (spec 11.5.1 step 4).
const (
	turnCompleted   = "COMPLETED"
	turnInterrupted = "INTERRUPTED"
)

var textReplacer = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", " ", " ", " ", " ", "<", "＜", ">", "＞")

// sanitizeText puts a text on one line and replaces '<' and '>' so that nothing inside it can
// close the <transcript> delimiter.
func sanitizeText(s string) string { return strings.TrimSpace(textReplacer.Replace(s)) }

// BuildTranscript renders turns as "[seq=N] USER: ..." and "[seq=N] AI: ..." lines (spec 11.5.1).
// The AI text of an INTERRUPTED turn is assistant_spoken_text. When the text exceeds maxRunes the
// oldest turns are dropped whole until it fits. validSeqs maps the seq of every kept turn with a
// non-empty user text to its id; userSeqs counts the turns of the whole input whose user text has
// at least 5 runes (the skip rule runs before the cut). Turns of another status are ignored.
func BuildTranscript(turns []TurnRow, maxRunes int) (text string, validSeqs map[int]uuid.UUID, userSeqs int) {
	rows := make([]TurnRow, 0, len(turns))
	for _, t := range turns {
		if t.Status == turnCompleted || t.Status == turnInterrupted {
			rows = append(rows, t)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })

	type block struct {
		text   string
		runes  int
		seq    int
		id     uuid.UUID
		hasUse bool
	}
	blocks := make([]block, 0, len(rows))
	for _, t := range rows {
		var lines []string
		user := ""
		if t.UserText != nil {
			user = sanitizeText(*t.UserText)
		}
		if utf8.RuneCountInString(user) >= minUserRunes {
			userSeqs++
		}
		if user != "" {
			lines = append(lines, fmt.Sprintf("[seq=%d] USER: %s", t.Seq, user))
		}
		ai := t.AssistantText
		if t.Status == turnInterrupted {
			ai = t.AssistantSpokenText
		}
		if ai = sanitizeText(ai); ai != "" {
			lines = append(lines, fmt.Sprintf("[seq=%d] AI: %s", t.Seq, ai))
		}
		if len(lines) == 0 {
			continue
		}
		b := strings.Join(lines, "\n")
		blocks = append(blocks, block{text: b, runes: utf8.RuneCountInString(b), seq: t.Seq, id: t.ID, hasUse: user != ""})
	}

	total := 0
	for _, b := range blocks {
		total += b.runes
	}
	total += max(0, len(blocks)-1) // newlines between blocks
	start := 0
	for start < len(blocks) && total > maxRunes {
		total -= blocks[start].runes + 1
		start++
	}
	kept := blocks[start:]
	validSeqs = map[int]uuid.UUID{}
	parts := make([]string, 0, len(kept))
	for _, b := range kept {
		parts = append(parts, b.text)
		if b.hasUse {
			validSeqs[b.seq] = b.id
		}
	}
	return strings.Join(parts, "\n"), validSeqs, userSeqs
}

// exampleOutput is the example output of spec 11.5.2.
const exampleOutput = `{
  "memories": [
    {"category":"PROFILE_FACT","key":"occupation","content_ja":"バックエンドエンジニアとして働いている。",
     "display_text_id":"Bekerja sebagai backend engineer.","evidence_type":"EXPLICIT_USER_STATEMENT",
     "confidence":0.95,"importance":0.9,"is_temporary":false,"valid_days":null,"source_turn_seqs":[1],"sensitive":false},
    {"category":"PROFILE_FACT","key":"plan:job_interview","content_ja":"来月、日本企業の面接を受ける予定。",
     "display_text_id":"Bulan depan akan wawancara kerja di perusahaan Jepang.","evidence_type":"EXPLICIT_USER_STATEMENT",
     "confidence":0.9,"importance":0.8,"is_temporary":true,"valid_days":45,"source_turn_seqs":[4],"sensitive":false},
    {"category":"LEARNING_PATTERN","key":"error:particle_wa_ga","content_ja":"「は」と「が」を混同することが多い。",
     "display_text_id":"Sering tertukar antara partikel は dan が.","evidence_type":"INFERRED",
     "confidence":0.8,"importance":0.7,"is_temporary":false,"valid_days":null,"source_turn_seqs":[3,7,9],"sensitive":false}
  ],
  "episode": {"content_ja":"面接練習（バックエンドエンジニア）。Goを選んだ理由とマイクロサービスの経験を説明した。",
              "display_text_id":"Latihan wawancara (backend engineer). Menjelaskan alasan memilih Go dan pengalaman microservices.",
              "confidence":0.9,"importance":0.6,"source_turn_seqs":[3,5,7]}
}`

// ExampleOutput returns the example output of spec 11.5.2; it passes the seeded output_schema.
func ExampleOutput() []byte { return []byte(exampleOutput) }

// PromptFixtures are the data values the publish validator renders the MEMORY_EXTRACTION templates
// against (spec 6.10.1): minimal (no existing memory, 3 turns) and maximal (150 existing memories,
// a transcript of 40,000 runes).
func PromptFixtures() []any {
	str := func(s string) *string { return &s }
	minTurns := []TurnRow{
		{ID: uuid.Nil, Seq: 0, Status: turnCompleted, AssistantText: "こんにちは！今日はどんな一日でしたか？"},
		{ID: uuid.Nil, Seq: 1, Status: turnCompleted, UserText: str("今日は仕事でした。バックエンドエンジニアです。"), AssistantText: "そうなんですね。どんな言語を使っていますか？"},
		{ID: uuid.Nil, Seq: 2, Status: turnCompleted, UserText: str("Goをよく使っています。"), AssistantText: "いいですね。"},
	}
	minText, _, _ := BuildTranscript(minTurns, TranscriptMaxRunes)
	minimal := MemoryExtractionPromptData{SessionDate: "2026-10-16", ScenarioTitle: "自己紹介", Transcript: minText}

	existing := make([]ExistingMemoryLine, 0, MaxExistingMemories)
	for i := range MaxExistingMemories {
		existing = append(existing, NewExistingMemoryLine(CategoryProfileFact, fmt.Sprintf("hobby:item_%03d", i),
			strings.Repeat("あ", 59)+"。"))
	}
	var turns []TurnRow
	for seq, n := 0, 0; n <= TranscriptMaxRunes+1000; seq++ {
		u := strings.Repeat("週末は友達と京都へ行きました。", 7)
		a := strings.Repeat("いいですね。どこを見ましたか？", 7)
		turns = append(turns, TurnRow{Seq: seq, Status: turnCompleted, UserText: &u, AssistantText: a})
		n += 2 * 105
	}
	maxText, _, _ := BuildTranscript(turns, TranscriptMaxRunes)
	maximal := MemoryExtractionPromptData{SessionDate: "2026-10-16", ScenarioTitle: strings.Repeat("ホテルの予約", 10),
		ExistingMemories: existing, Transcript: maxText}
	return []any{minimal, maximal}
}
