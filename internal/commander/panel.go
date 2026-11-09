package commander

import (
	"github.com/drolosoft/gotify-commander/internal/command"
	"github.com/drolosoft/gotify-commander/internal/config"
)

// PanelCommand is one command as the control panel offers it in its prompt.
type PanelCommand struct {
	Name    string `json:"name"`
	Usage   string `json:"usage"`
	Summary string `json:"summary"`

	// Target is what goes after the command: "machine", "service",
	// "container", "coordinates" or "" for nothing.
	Target   string `json:"target"`
	Required bool   `json:"required"`

	// Confirm is true when the panel asks before it runs the command.
	Confirm bool `json:"confirm"`
}

// PanelCommands returns the commands cfg lets run, described for the
// control panel. A command left out of enabled_commands is not listed,
// because it would only be answered as disabled.
//
// The panel asks before the dangerous commands (reboot, shutdown, restarting
// a container and every command of a category with danger: true) and before
// the ones in require_confirm: a message has to confirm those with a code,
// and a click is easier to get wrong than a message.
func PanelCommands(cfg *config.Config) []PanelCommand {
	enabled := enabledActions(cfg)
	dangerous := dangerousActions(cfg)
	confirmed := actionSet(cfg.RequireConfirm)

	catalog := command.Catalog()
	commands := make([]PanelCommand, 0, len(catalog))

	for _, info := range catalog {
		// A nil set means enabled_commands is empty and everything runs.
		if enabled != nil && !enabled[info.Action] {
			continue
		}

		commands = append(commands, PanelCommand{
			Name:     string(info.Action),
			Usage:    info.Usage,
			Summary:  info.Summary,
			Target:   string(info.Target),
			Required: info.Required,
			Confirm:  dangerous[info.Action] || confirmed[info.Action],
		})
	}

	return commands
}
