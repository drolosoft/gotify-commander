package security

import (
	"errors"
	"regexp"
)

var validInput = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ValidateInput checks that the input string is safe and well-formed.
// It returns an error if the input is empty or contains disallowed characters.
func ValidateInput(s string) error {
	if s == "" {
		return errors.New("input must not be empty")
	}
	if !validInput.MatchString(s) {
		return errors.New("input contains invalid characters")
	}
	return nil
}
