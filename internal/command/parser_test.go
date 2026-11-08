package command

import (
	"testing"
)

var testAliases = map[string]string{
	"nginx":   "nginx",
	"ng":      "nginx",
	"web":     "nginx",
	"laporra": "laporra",
	"porra":   "laporra",
	"lp":      "laporra",
}

var testServices = map[string]bool{
	"nginx":   true,
	"laporra": true,
}

func TestParse(t *testing.T) {
	cases := []struct {
		input          string
		expectedAction Action
		expectedTarget string
		expectError    bool
	}{
		{"restart nginx", ActionRestart, "nginx", false},
		{"restart ng", ActionRestart, "nginx", false},
		{"stop porra", ActionStop, "laporra", false},
		{"status", ActionStatus, "", false},
		{"status lp", ActionStatus, "laporra", false},
		{"reboot vps", ActionReboot, "vps", false},
		{"free", ActionFree, "vps", false},
		{"free mac", ActionFree, "mac", false},
		{"df", ActionDf, "vps", false},
		{"logs nginx", ActionLogs, "nginx", false},
		{"uptime", ActionUptime, "vps", false},
		{"ping mac", ActionPing, "mac", false},
		{"services", ActionServices, "", false},
		{"help", ActionHelp, "", false},
		{"mem", ActionFree, "vps", false},
		{"disk", ActionDf, "vps", false},
		{"log nginx", ActionLogs, "nginx", false},
		{"up", ActionUptime, "vps", false},
		{"?", ActionHelp, "", false},
		{"nginx", ActionStatus, "nginx", false},
		{"porra", ActionStatus, "laporra", false},
		{"RESTART Nginx", ActionRestart, "nginx", false},
		{"confirm 4821", ActionConfirm, "4821", false},
		{"Confirm 4821 please", ActionConfirm, "4821", false},
		{"confirm", ActionConfirm, "", false},
		{"confirm ng", ActionConfirm, "ng", false},
		{"unknowncmd", "", "", true},
		{"", "", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			cmd, err := Parse(tc.input, testAliases, testServices, "confirm")
			if tc.expectError {
				if err == nil {
					t.Errorf("input %q: expected error, got none (action=%s, target=%s)", tc.input, cmd.Action, cmd.Target)
				}
				return
			}
			if err != nil {
				t.Errorf("input %q: unexpected error: %v", tc.input, err)
				return
			}
			if cmd.Action != tc.expectedAction {
				t.Errorf("input %q: action = %q, want %q", tc.input, cmd.Action, tc.expectedAction)
			}
			if cmd.Target != tc.expectedTarget {
				t.Errorf("input %q: target = %q, want %q", tc.input, cmd.Target, tc.expectedTarget)
			}
		})
	}
}

// TestParseConfirmationKeyword: the reply word comes from the configuration,
// an empty keyword means confirmation is off, and the keyword is read before
// the command aliases.
func TestParseConfirmationKeyword(t *testing.T) {
	cases := []struct {
		input       string
		keyword     string
		action      Action
		target      string
		expectError bool
	}{
		{input: "ok 1234", keyword: "ok", action: ActionConfirm, target: "1234"},
		{input: "OK 1234", keyword: "ok", action: ActionConfirm, target: "1234"},
		{input: "confirm 1234", keyword: "ok", expectError: true},
		{input: "confirm 1234", keyword: "", expectError: true},
		{input: "up 1234", keyword: "up", action: ActionConfirm, target: "1234"},
		{input: "up", keyword: "", action: ActionUptime, target: "vps"},
	}

	for _, tc := range cases {
		cmd, err := Parse(tc.input, testAliases, testServices, tc.keyword)
		if tc.expectError {
			if err == nil {
				t.Errorf("Parse(%q, keyword %q) = %+v, want an error", tc.input, tc.keyword, cmd)
			}
			continue
		}

		if err != nil {
			t.Errorf("Parse(%q, keyword %q): %v", tc.input, tc.keyword, err)
			continue
		}
		if cmd.Action != tc.action || cmd.Target != tc.target {
			t.Errorf("Parse(%q, keyword %q) = %q %q, want %q %q", tc.input, tc.keyword, cmd.Action, cmd.Target, tc.action, tc.target)
		}
	}
}
