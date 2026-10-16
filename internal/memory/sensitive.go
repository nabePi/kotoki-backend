package memory

import (
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// SensitiveHit names the reason of a rejected candidate.
type SensitiveHit struct{ Reason string }

// Reasons of the sensitive filter (spec 11.5.3 rule 6 and 11.5.4).
const (
	ReasonSensitiveHealth      = "SENSITIVE_HEALTH"
	ReasonSensitiveReligion    = "SENSITIVE_RELIGION"
	ReasonSensitivePolitics    = "SENSITIVE_POLITICS"
	ReasonSensitiveSexuality   = "SENSITIVE_SEXUALITY"
	ReasonSensitiveEthnicity   = "SENSITIVE_ETHNICITY"
	ReasonSensitiveCriminal    = "SENSITIVE_CRIMINAL"
	ReasonSensitiveImmigration = "SENSITIVE_IMMIGRATION"
	ReasonSensitiveFinance     = "SENSITIVE_FINANCE"
	ReasonSensitiveIdentifier  = "SENSITIVE_IDENTIFIER"
	ReasonSensitiveBirthdate   = "SENSITIVE_BIRTHDATE"
	ReasonSensitiveThirdParty  = "SENSITIVE_THIRD_PARTY"
)

type termList struct {
	reason string
	terms  []string // lowercase, NFKC
}

// sensitiveTerms is spec 11.5.4, in the fixed evaluation order; the first category that matches
// wins. False positives are accepted (spec): not storing is better than storing.
var sensitiveTerms = []termList{
	{ReasonSensitiveHealth, []string{"病気", "病名", "診断", "治療", "手術", "入院", "通院", "薬を", "服用", "うつ", "鬱", "精神",
		"メンタル", "障害", "障がい", "妊娠", "がん", "癌", "アレルギー", "sakit", "penyakit", "obat", "rawat inap", "hamil",
		"depresi", "disabilitas", "diagnosis", "medication", "pregnant", "therapy"}},
	{ReasonSensitiveReligion, []string{"宗教", "信仰", "キリスト教", "イスラム", "ムスリム", "仏教徒", "神道", "教会に", "モスク",
		"agama", "beragama", "islam", "kristen", "katolik", "hindu", "buddha", "religion", "church"}},
	{ReasonSensitivePolitics, []string{"政党", "支持政党", "自民党", "選挙で", "政治的", "partai", "pilpres", "pemilu", "politik",
		"political party"}},
	{ReasonSensitiveSexuality, []string{"同性愛", "ゲイ", "レズ", "バイセクシュアル", "lgbt", "性的指向", "トランスジェンダー", "性生活",
		"orientasi seksual", "transgender"}},
	{ReasonSensitiveEthnicity, []string{"民族", "人種", "部落", "suku", "ras", "etnis", "ethnicity", "race"}},
	{ReasonSensitiveCriminal, []string{"逮捕", "前科", "犯罪歴", "刑務所", "裁判", "訴訟", "ditangkap", "penjara", "pidana", "arrested",
		"lawsuit"}},
	{ReasonSensitiveImmigration, []string{"ビザ", "在留資格", "不法滞在", "難民", "visa", "imigrasi", "izin tinggal"}},
	{ReasonSensitiveFinance, []string{"年収", "月収", "給料は", "借金", "ローン", "口座", "クレジットカード", "貯金", "gaji", "utang",
		"hutang", "pinjaman", "rekening", "salary", "debt"}},
	{ReasonSensitiveIdentifier, []string{"パスワード", "暗証番号", "マイナンバー", "パスポート番号", "住所は", "番地", "丁目", "電話番号",
		"メールアドレス", "nik", "ktp", "password"}},
	{ReasonSensitiveBirthdate, []string{"生年月日", "誕生日は", "tanggal lahir"}},
}

type patternRule struct {
	reason string
	re     *regexp.Regexp
}

// sensitivePatterns run after all term lists. The full-date pattern precedes the phone pattern:
// "2000-01-31" also fits the phone shape and must be reported as a birthdate.
var sensitivePatterns = []patternRule{
	{ReasonSensitiveIdentifier, regexp.MustCompile(`[^\s@]+@[^\s@]+\.[^\s@]+`)},
	{ReasonSensitiveBirthdate, regexp.MustCompile(`\d{4}[年/\-]\d{1,2}[月/\-]\d{1,2}`)},
	{ReasonSensitiveIdentifier, regexp.MustCompile(`\+?\d[\d\- ]{8,}\d`)},
	{ReasonSensitiveIdentifier, regexp.MustCompile(`\d{6,}`)},
}

var thirdPartyNameRE = regexp.MustCompile(`[\p{Katakana}\p{Han}]{2,}(さん|くん|ちゃん)`)

// foldText is NFKC plus lowercase, the comparison form of every filter.
func foldText(s string) string { return strings.ToLower(norm.NFKC.String(s)) }

// keyWords turns a key into space-separated words so that ASCII terms match whole words only:
// "strength:keigo_set_phrases" must not hit the term "ras" ("phrases").
func keyWords(key string) string {
	return " " + strings.NewReplacer(":", " ", "_", " ", "-", " ").Replace(foldText(key)) + " "
}

func hasTerm(text, words, term string) bool {
	if strings.Contains(text, term) {
		return true
	}
	if words == "" {
		return false
	}
	if isASCII(term) {
		return strings.Contains(words, " "+term+" ")
	}
	return strings.Contains(words, term)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// CheckSensitive applies the deterministic sensitive-data filter (spec 11.5.3 rule 6, 11.5.4) to a
// candidate: NFKC, case-insensitive substring terms per category in a fixed order, the email,
// phone, 6-digit and full-date patterns, and a third-party-name heuristic for family: keys. The
// key is matched by whole words; content and display text by substring. The key of an EPISODIC
// candidate is server-assigned and not checked. hit is true with the first matching reason.
func CheckSensitive(c Category, key, contentJA, displayID string) (reason string, hit bool) {
	text := foldText(contentJA + "\n" + displayID)
	words := ""
	if c != CategoryEpisodic {
		words = keyWords(key)
	}
	for _, tl := range sensitiveTerms {
		for _, term := range tl.terms {
			if hasTerm(text, words, term) {
				return tl.reason, true
			}
		}
	}
	scan := text
	if words != "" {
		scan += "\n" + strings.TrimSpace(foldText(key))
	}
	for _, p := range sensitivePatterns {
		if p.re.MatchString(scan) {
			return p.reason, true
		}
	}
	if strings.HasPrefix(foldText(key), "family:") && thirdPartyNameRE.MatchString(contentJA) {
		return ReasonSensitiveThirdParty, true
	}
	return "", false
}

var injectionRE = regexp.MustCompile(`(?i)ignore (all|previous|the) (instructions|rules)|system prompt|指示を無視|ルールを無視|プロンプト|abaikan (instruksi|perintah)`)

// CheckInjection reports whether the candidate looks like an instruction aimed at the AI (spec
// 11.5.3 rule 7). Key separators count as spaces so that "ignore_previous_instructions" is caught.
func CheckInjection(key, contentJA, displayID string) bool {
	return injectionRE.MatchString(foldText(contentJA+"\n"+displayID)) ||
		injectionRE.MatchString(strings.TrimSpace(keyWords(key)))
}
