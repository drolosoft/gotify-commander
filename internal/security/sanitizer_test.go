package security

import (
	"testing"
)

func TestValidateInput_Valid(t *testing.T) {
	valid := []string{
		"nginx",
		"laporra",
		"pre-laporra",
		"nginx_proxy",
		"com.myapp.service",
		"redis-server",
		"my123",
	}
	for _, s := range valid {
		t.Run(s, func(t *testing.T) {
			if err := ValidateInput(s); err != nil {
				t.Errorf("expected %q to be valid, got error: %v", s, err)
			}
		})
	}
}

func TestValidateInput_Invalid(t *testing.T) {
	invalid := []string{
		"",
		"nginx;rm -rf /",
		"service && echo pwned",
		"$(whoami)",
		"`id`",
		"nginx\nnewline",
		"../etc/passwd",
		"service | grep",
		"name with spaces",
	}
	for _, s := range invalid {
		t.Run(s, func(t *testing.T) {
			if err := ValidateInput(s); err == nil {
				t.Errorf("expected %q to be invalid, got no error", s)
			}
		})
	}
}
