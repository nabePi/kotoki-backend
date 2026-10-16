package prompt

import "fmt"

// Japanese adaptation descriptors of spec 12.8.4. The text is code, not content: changing it
// is a code change. The tables are keyed by primitives so prompt never imports learner.

// ClosingLine ends every <conversation_difficulty> block (evaluation is post-session only).
const ClosingLine = "学習者の間違いを指摘・訂正しないでください。"

var dimensionDescriptors = map[string][5]string{
	"vocabulary": {
		"日常の基本語彙だけを使う（N5程度）。",
		"基本的な語彙を使う（N4程度）。専門用語は使わない。",
		"一般的な語彙を使う（N3程度）。専門用語には簡単な言い換えを添える。",
		"幅広い語彙を使う（N2程度）。場面に合う専門用語を使ってよい。",
		"語彙の制限なし。慣用句や専門用語も自然に使う。",
	},
	"grammar": {
		"短い単文だけ。です・ます形と「て形」程度。",
		"簡単な複文（〜から、〜けど、〜たい）まで。",
		"一般的な複文（〜ので、〜ながら、〜たら、受身）を使う。",
		"複雑な文型（〜わけではない、〜ようにする、使役受身）も使う。",
		"文法の制限なし。自然な省略や倒置も使う。",
	},
	"speech_speed": {
		"一文を15文字程度までに区切る。",
		"一文を25文字程度までにする。",
		"普通の長さの文で話す。",
		"自然な長さの文で話す。",
		"自然な長さで、縮約形（〜ちゃう、〜てる）も使う。",
	},
	"topic_novelty": {
		"学習者がよく知っている身近な内容だけを扱う。",
		"身近な内容を中心に、少しだけ新しい面に触れる。",
		"話題の中で新しい面を一つ取り入れてよい。",
		"過去の会話で扱っていない面を積極的に取り入れる。",
		"予想外の関連話題や専門的な面にも踏み込む。",
	},
	"topic_abstraction": {
		"目の前の具体的なこと（物・行動・予定）だけを話す。",
		"具体的な経験や事実について話す。",
		"理由や比較を少し尋ねる。",
		"意見、理由、メリット・デメリットを尋ねる。",
		"抽象的な概念、仮定、社会的な議論も扱う。",
	},
	"question_complexity": {
		"はい/いいえ、または選択肢のある質問だけをする。",
		"何・どこ・いつ などの簡単な質問をする。",
		"「どうでしたか」「どんな〜」などの開かれた質問をする。",
		"理由や方法を説明させる質問（なぜ、どうやって）をする。",
		"仮定や複数の要素を含む質問をする。",
	},
	"follow_up_depth": {
		"同じ点を深掘りせず、次の話題へ進む。",
		"同じ点への追加質問は1回まで。",
		"追加質問は2回まで。",
		"追加質問は3回まで。具体例を求めてよい。",
		"追加質問は3回まで。詳細や食い違いも掘り下げてよい。",
	},
	"expected_response_length": {
		"一言か短いフレーズで答えられる質問をする。",
		"一文で答えられる質問をする。",
		"1〜2文の答えを引き出す。",
		"2〜3文の説明を引き出す。",
		"4文以上のまとまった説明を引き出す。",
	},
	"spontaneity": {
		"予測しやすい流れで話し、次に話すことを予告する。",
		"基本的に予測しやすい流れで話す。",
		"ときどき自然に話題を変える。",
		"予告なしの質問や話題転換をしてよい。",
		"実際の会話のように予想外の展開も入れる。",
	},
	"turn_taking": {
		"毎回、明確な質問で発話を終える。",
		"ほとんどの発話を質問で終える。",
		"ときどき質問なしのコメントで終え、学習者が続けるのを待つ。",
		"相づちや短い反応で学習者に主導権を渡し、学習者からの質問を促す。",
		"相づち・短いコメント・話題提示を自然に混ぜ、学習者が会話を主導することを期待する。",
	},
}

// DimensionDescriptors lists the dimension keys in the fixed render order.
var DimensionDescriptors = []string{
	"vocabulary", "grammar", "speech_speed", "topic_novelty", "topic_abstraction",
	"question_complexity", "follow_up_depth", "expected_response_length", "spontaneity", "turn_taking",
}

var scaffoldingDescriptors = [4]string{
	"支援はしない。自然な会話相手として振る舞う。",
	"学習者が助けを求めたときだけ、簡単な日本語で言い換える。",
	"学習者が詰まったり聞き返したりしたら、簡単な日本語で言い換えるか選択肢を示す。",
	"質問に答え方の例や選択肢を添える。詰まったら文の出だしを示す。言い換えは常に簡単な日本語で。",
}

var formalityDescriptors = map[string]string{
	"CASUAL":      "普通体（タメ口）で話す。",
	"POLITE":      "です・ます体で話す。",
	"KEIGO_LIGHT": "役割上の決まった敬語表現は使うが、それ以外はわかりやすいです・ます体で話す。",
	"KEIGO":       "役割にふさわしい尊敬語・謙譲語を正しく使う。",
}

var dialectDescriptors = map[string]string{
	"NONE":  "標準語で話す。",
	"LIGHT": "基本は標準語で、関西弁の文末表現や頻出語（〜やん、〜ねん、めっちゃ、ほんま）を一つの発話に一つ程度まぜる。",
	"FULL":  "自然な関西弁で話す。",
}

var focusDescriptors = map[string]string{
	"GRAMMAR":                "学習者が文を組み立てる必要のある質問（理由・経験・予定）を自然に入れる。",
	"VOCABULARY":             "話題に関係する具体的な物事について尋ね、学習者が語彙を使う機会を作る。",
	"FLUENCY":                "「もう少し詳しく教えてください」などで、学習者が続けて話せるように促す。",
	"NATURALNESS":            "自然な言い回しや相づちを自分の発話の中で示す。",
	"LISTENING_RESPONSE":     "数字・理由・条件など、理解して答える必要のある情報を含めて話す。",
	"INTERACTION_COMPLEXITY": "学習者が質問したり意見を述べたりする機会を作る。",
}

var goalDescriptors = map[string]string{
	"DAILY_CONVERSATION": "日常会話の練習",
	"JOB_INTERVIEW":      "日本での就職面接に向けた練習",
	"BUSINESS":           "仕事で使う日本語の練習",
	"TRAVEL":             "旅行で使う日本語の練習",
	"EXAM_PREP":          "試験対策としての会話練習",
	"LIVING_IN_JAPAN":    "日本での生活に必要な会話の練習",
}

// DimensionDescriptor returns the descriptor of dim at level 1..5 (levels outside the range
// are clamped); an unknown dim gives "".
func DimensionDescriptor(dim string, level int) string {
	d, ok := dimensionDescriptors[dim]
	if !ok {
		return ""
	}
	return d[min(max(level, 1), 5)-1]
}

// ScaffoldingDescriptor returns the descriptor of scaffolding 0..3 (clamped).
func ScaffoldingDescriptor(n int) string { return scaffoldingDescriptors[min(max(n, 0), 3)] }

// FormalityDescriptor returns the descriptor of CASUAL, POLITE, KEIGO_LIGHT or KEIGO; else "".
func FormalityDescriptor(f string) string { return formalityDescriptors[f] }

// DialectDescriptor returns the descriptor of NONE, LIGHT or FULL; else "".
func DialectDescriptor(d string) string { return dialectDescriptors[d] }

// FocusDescriptor returns the descriptor of a focus skill name; else "".
func FocusDescriptor(skill string) string { return focusDescriptors[skill] }

// MaxSentencesLine is the sentence cap line of the block.
func MaxSentencesLine(n int) string {
	return fmt.Sprintf("一回の発話は%d文以内にする。", n)
}

// GoalDescriptor returns the Japanese descriptor of a learning_goal; OTHER, "" and unknown
// values give "" (no goal line).
func GoalDescriptor(goal string) string { return goalDescriptors[goal] }
