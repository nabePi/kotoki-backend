package prompt_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/prompt"
)

func TestFormalityInstructionTable(t *testing.T) {
	require.Equal(t, "Use casual speech (plain forms, タメ口) like with a friend.", prompt.FormalityInstruction("CASUAL"))
	require.Equal(t, "Use polite です/ます forms.", prompt.FormalityInstruction("POLITE"))
	require.Equal(t, "Use polite です/ます forms; use the fixed keigo expressions your role calls for, but keep everything else plain and easy to follow.", prompt.FormalityInstruction("KEIGO_LIGHT"))
	require.Equal(t, "Use respectful keigo (尊敬語 and 謙譲語) appropriate to a formal setting.", prompt.FormalityInstruction("KEIGO"))
	require.Equal(t, prompt.FormalityInstruction("POLITE"), prompt.FormalityInstruction("SOMETHING_ELSE"), "unknown values fall back to POLITE")
	require.Equal(t, prompt.FormalityInstruction("POLITE"), prompt.FormalityInstruction(""))
}
