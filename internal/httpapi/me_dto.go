package httpapi

import (
	"time"

	"github.com/nabePi/kotoki-backend/internal/auth"
)

const timeLayout = "2006-01-02T15:04:05.000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := formatTime(*t)
	return &s
}

// meResponse is the Me object of spec 5.9.2.
type meResponse struct {
	ID                  string  `json:"id"`
	Email               string  `json:"email"`
	FullName            string  `json:"full_name"`
	PhoneNumber         *string `json:"phone_number"`
	Role                string  `json:"role"`
	Status              string  `json:"status"`
	TermsVersion        string  `json:"terms_version"`
	TermsCurrentVersion string  `json:"terms_current_version"`
	TermsAcceptedAt     string  `json:"terms_accepted_at"`
	CreatedAt           string  `json:"created_at"`
	LastLoginAt         *string `json:"last_login_at"`
}

func newMeResponse(u auth.User, termsCurrent string) meResponse {
	return meResponse{
		ID: u.ID.String(), Email: u.Email, FullName: u.FullName, PhoneNumber: u.PhoneNumber,
		Role: string(u.Role), Status: string(u.Status), TermsVersion: u.TermsVersion, TermsCurrentVersion: termsCurrent,
		TermsAcceptedAt: formatTime(u.TermsAcceptedAt), CreatedAt: formatTime(u.CreatedAt), LastLoginAt: formatTimePtr(u.LastLoginAt),
	}
}

type consentsRequest struct {
	TermsVersion string `json:"terms_version"`
}

func (r consentsRequest) Validate() error {
	if r.TermsVersion == "" {
		return FieldError("terms_version", "required")
	}
	return nil
}

type deleteAccountRequest struct {
	Password string `json:"password"`
}

func (r deleteAccountRequest) Validate() error {
	if r.Password == "" {
		return FieldError("password", "required")
	}
	return nil
}
