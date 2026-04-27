package command

import (
	"errors"
	"strings"
)

// commandAliases maps input words to canonical Actions.
var commandAliases = map[string]Action{
	"restart":      ActionRestart,
	"stop":         ActionStop,
	"start":        ActionStart,
	"status":       ActionStatus,
	"reboot":       ActionReboot,
	"free":         ActionFree,
	"mem":          ActionFree,
	"memory":       ActionFree,
	"df":           ActionDf,
	"disk":         ActionDf,
	"space":        ActionDf,
	"logs":         ActionLogs,
	"log":          ActionLogs,
	"uptime":       ActionUptime,
	"up":           ActionUptime,
	"who":          ActionWho,
	"ping":         ActionPing,
	"services":     ActionServices,
	"list":         ActionServices,
	"help":         ActionHelp,
	"?":            ActionHelp,
	"shutdown":     ActionShutdown,
	"poweroff":     ActionShutdown,
	"top":          ActionTop,
	"processes":    ActionTop,
	"ps":           ActionTop,
	"ports":        ActionPorts,
	"listening":    ActionPorts,
	"ip":           ActionIp,
	"address":      ActionIp,
	"addr":         ActionIp,
	"updates":      ActionUpdates,
	"upgrade":      ActionUpdates,
	"apt":          ActionUpdates,
	"certs":        ActionCerts,
	"ssl":          ActionCerts,
	"certificates": ActionCerts,
	"connections":  ActionConnections,
	"conn":         ActionConnections,
	"sockets":      ActionConnections,
	"traffic":      ActionTraffic,
	"hits":         ActionTraffic,
	"rhit":         ActionTraffic,
	"visits":       ActionTraffic,
	"analytics":    ActionAnalytics,
	"goaccess":     ActionAnalytics,
	"stats":        ActionAnalytics,
	"locate":       ActionLocate,
	"where":        ActionLocate,
	"location":     ActionLocate,
}

// systemCommands are actions that default their target to "vps" when no target is given.
var systemCommands = map[Action]bool{
	ActionFree:        true,
	ActionDf:          true,
	ActionUptime:      true,
	ActionWho:         true,
	ActionPing:        true,
	ActionReboot:      true,
	ActionShutdown:    true,
	ActionTop:         true,
	ActionPorts:       true,
	ActionIp:          true,
	ActionConnections: true,
}

// requiresTarget are system commands that must have an explicit target (no default for safety).
var requiresTarget = map[Action]bool{
	ActionShutdown: true,
	ActionReboot:   true,
}

// noTargetCommands are actions that naturally have no target.
var noTargetCommands = map[Action]bool{
	ActionStatus:    true,
	ActionServices:  true,
	ActionHelp:      true,
	ActionUpdates:   true,
	ActionCerts:     true,
	ActionAnalytics: true,
	ActionLocate:    true,
}

// Parse parses a raw input string into a Command.
//
// aliases maps short names / nicknames to canonical service names.
// services is the set of known service names for smart fallback (bare name → status).
func Parse(input string, aliases map[string]string, services map[string]bool) (Command, error) {
	raw := input
	input = strings.TrimSpace(input)
	if input == "" {
		return Command{}, errors.New("empty input")
	}

	parts := strings.Fields(input)
	verb := strings.ToLower(parts[0])
	args := parts[1:]

	// Lower-case all args for consistency
	for i, a := range args {
		args[i] = strings.ToLower(a)
	}

	// Check if verb is a known command action
	if action, ok := commandAliases[verb]; ok {
		target := resolveTarget(action, args, aliases)
		return Command{Action: action, Target: target, Raw: raw}, nil
	}

	// Smart fallback: bare service name → status
	// Check if verb (possibly aliased) resolves to a known service
	resolved := verb
	if alias, ok := aliases[verb]; ok {
		resolved = alias
	}
	if services[resolved] {
		return Command{Action: ActionStatus, Target: resolved, Raw: raw}, nil
	}

	return Command{}, errors.New("unknown command: " + verb)
}

// resolveTarget determines the target for an action given its args and alias map.
func resolveTarget(action Action, args []string, aliases map[string]string) string {
	// Actions with no meaningful target
	if noTargetCommands[action] {
		if len(args) > 0 {
			// status can take a target
			if action == ActionStatus {
				name := args[0]
				if alias, ok := aliases[name]; ok {
					return alias
				}
				return name
			}
		}
		return ""
	}

	// System commands: default target is "vps" (unless requiresTarget)
	if systemCommands[action] {
		if len(args) == 0 {
			if requiresTarget[action] {
				return "" // handler will reject the missing target
			}
			return "vps"
		}
		name := args[0]
		if alias, ok := aliases[name]; ok {
			return alias
		}
		return name
	}

	// Service commands (restart, stop, start, logs): resolve via aliases
	if len(args) == 0 {
		return ""
	}
	name := args[0]
	if alias, ok := aliases[name]; ok {
		return alias
	}
	return name
}
