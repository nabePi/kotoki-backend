package httpapi

import (
	"strings"
	"time"

	"github.com/nabePi/kotoki-backend/internal/auth"
)

type registerRequest struct {
	Email        string  `json:"email"`
	Password     string  `json:"password"`
	FullName     string  `json:"full_name"`
	PhoneNumber  *string `json:"phone_number"`
	TermsVersion string  `json:"terms_version"`
}

func (r registerRequest) Validate() error {
	switch {
	case strings.TrimSpace(r.Email) == "":
		return FieldError("email", "required")
	case r.Password == "":
		return FieldError("password", "required")
	case strings.TrimSpace(r.FullName) == "":
		return FieldError("full_name", "required")
	case r.TermsVersion == "":
		return FieldError("terms_version", "required")
	}
	return nil
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (r loginRequest) Validate() error {
	if strings.TrimSpace(r.Email) == "" {
		return FieldError("email", "required")
	}
	if r.Password == "" {
		return FieldError("password", "required")
	}
	return nil
}

type forgotRequest struct {
	Email string `json:"email"`
}

func (r forgotRequest) Validate() error {
	if strings.TrimSpace(r.Email) == "" {
		return FieldError("email", "required")
	}
	return nil
}

type resetRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

func (r resetRequest) Validate() error {
	if r.Token == "" {
		return FieldError("token", "required")
	}
	if r.NewPassword == "" {
		return FieldError("new_password", "required")
	}
	return nil
}

type authResponse struct {
	AccessToken string     `json:"access_token"`
	TokenType   string     `json:"token_type"`
	ExpiresIn   int        `json:"expires_in"`
	User        meResponse `json:"user"`
}

type refreshResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

type statusResponse struct {
	Status string `json:"status"`
}

type termsResponse struct {
	TermsVersion string `json:"terms_version"`
}

func newAuthResponse(r auth.AuthResult, termsCurrent string) authResponse {
	return authResponse{
		AccessToken: r.AccessToken, TokenType: "Bearer", ExpiresIn: int(r.ExpiresIn / time.Second),
		User: newMeResponse(r.User, termsCurrent),
	}
}
