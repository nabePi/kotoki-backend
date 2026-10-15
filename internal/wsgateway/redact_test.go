package wsgateway_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/wsgateway"
)

func TestRedactTicket(t *testing.T) {
	for in, want := range map[string]string{
		"ticket=abc&x=1":             "ticket=REDACTED&x=1",
		"x=1&ticket=abc":             "x=1&ticket=REDACTED",
		"ticket=abc":                 "ticket=REDACTED",
		"":                           "",
		"x=1&y=2":                    "x=1&y=2",
		"ticket=a&ticket=b":          "ticket=REDACTED&ticket=REDACTED",
		"Ticket=abc":                 "Ticket=REDACTED",
		"tick%65t=abc&x=1":           "tick%65t=REDACTED&x=1",
		"ticket":                     "ticket=REDACTED",
		"ticket=":                    "ticket=REDACTED",
		"ticket=a%3Db&x=1":           "ticket=REDACTED&x=1",
		"myticket=abc&ticketx=1":     "myticket=abc&ticketx=1",
		"x=ticket=abc":               "x=ticket=abc",
		"a=1&ticket=abc&b=%E3%81%82": "a=1&ticket=REDACTED&b=%E3%81%82",
	} {
		require.Equal(t, want, wsgateway.RedactQuery(in), in)
	}
	// The documented example (§7.3, Task 24).
	require.Equal(t, "ticket=REDACTED&x=1", wsgateway.RedactQuery("ticket=abc&x=1"))
}
