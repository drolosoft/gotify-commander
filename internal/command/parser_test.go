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
		{"unknowncmd", "", "", true},
		{"", "", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			cmd, err := Parse(tc.input, testAliases, testServices)
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
