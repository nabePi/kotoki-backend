package prompt

// Formality values of Speech.Formality.
const (
	FormalityCasual    = "CASUAL"
	FormalityPolite    = "POLITE"
	FormalityKeigoLite = "KEIGO_LIGHT"
	FormalityKeigo     = "KEIGO"
)

var formalityInstructions = map[string]string{
	FormalityCasual:    "Use casual speech (plain forms, タメ口) like with a friend.",
	FormalityPolite:    "Use polite です/ます forms.",
	FormalityKeigoLite: "Use polite です/ます forms; use the fixed keigo expressions your role calls for, but keep everything else plain and easy to follow.",
	FormalityKeigo:     "Use respectful keigo (尊敬語 and 謙譲語) appropriate to a formal setting.",
}

// FormalityInstruction is the English instruction for a formality level (spec 6.8.4). An
// unknown value gets the POLITE text. Changing this table is a code change, not content.
func FormalityInstruction(f string) string {
	if s, ok := formalityInstructions[f]; ok {
		return s
	}
	return formalityInstructions[FormalityPolite]
}
