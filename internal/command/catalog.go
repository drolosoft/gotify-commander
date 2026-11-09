package command

// TargetKind says what a command takes after its name. The control panel
// reads it to know what to suggest once the command is typed.
type TargetKind string

const (
	// TargetNone is a command that runs as it is.
	TargetNone TargetKind = ""

	// TargetMachine is one of the machines of the configuration: vps or mac.
	TargetMachine TargetKind = "machine"

	// TargetService is a service of the configuration, by name or alias.
	TargetService TargetKind = "service"

	// TargetContainer is a name from docker.allowed_containers.
	TargetContainer TargetKind = "container"

	// TargetCoordinates is a latitude and a longitude. The panel fills
	// them from the browser's position.
	TargetCoordinates TargetKind = "coordinates"
)

// Info describes one registered command for whoever has to offer it: what to
// type, what it does and what goes after its name.
type Info struct {
	Action Action

	// Usage is what the user types, with the target in <angle brackets>
	// when it is required and in [square brackets] when it is optional.
	// The words before the first bracket are the command itself, so
	// docker-restart reads "docker restart <container>".
	Usage string

	// Summary is one short sentence, without the emoji of the help table.
	Summary string

	Target TargetKind

	// Required is false when the command also runs without a target.
	Required bool
}

// Catalog returns one Info per registered command, in the order the help
// table lists them. The commander's tests compare it with the registry, so a
// new command cannot be registered without being described here.
func Catalog() []Info {
	return []Info{
		{Action: ActionRestart, Usage: "restart <service>", Summary: "Restart a service", Target: TargetService, Required: true},
		{Action: ActionStop, Usage: "stop <service>", Summary: "Stop a service", Target: TargetService, Required: true},
		{Action: ActionStart, Usage: "start <service>", Summary: "Start a service", Target: TargetService, Required: true},
		{Action: ActionStatus, Usage: "status [service]", Summary: "Status of one service, or of all of them", Target: TargetService},
		{Action: ActionLogs, Usage: "logs <service>", Summary: "Last lines of a service's log", Target: TargetService, Required: true},
		{Action: ActionFree, Usage: "free [machine]", Summary: "Memory usage", Target: TargetMachine},
		{Action: ActionDf, Usage: "df [machine]", Summary: "Disk usage", Target: TargetMachine},
		{Action: ActionUptime, Usage: "uptime [machine]", Summary: "Uptime and load average", Target: TargetMachine},
		{Action: ActionTop, Usage: "top [machine]", Summary: "Top processes by CPU and memory", Target: TargetMachine},
		{Action: ActionWho, Usage: "who [machine]", Summary: "Logged-in users", Target: TargetMachine},
		{Action: ActionPing, Usage: "ping [machine]", Summary: "Connectivity check", Target: TargetMachine},
		{Action: ActionIp, Usage: "ip [machine]", Summary: "IP addresses", Target: TargetMachine},
		{Action: ActionPorts, Usage: "ports [machine]", Summary: "Listening ports", Target: TargetMachine},
		{Action: ActionConnections, Usage: "connections [machine]", Summary: "Socket statistics", Target: TargetMachine},
		{Action: ActionTraffic, Usage: "traffic [service]", Summary: "Nginx traffic of one site, or of all of them", Target: TargetService},
		{Action: ActionAnalytics, Usage: "analytics", Summary: "Web analytics"},
		{Action: ActionCerts, Usage: "certs", Summary: "Days left on each TLS certificate"},
		{Action: ActionUpdates, Usage: "updates [machine]", Summary: "Pending package updates", Target: TargetMachine},
		{Action: ActionLocate, Usage: "locate <lat> <lon>", Summary: "Address of a GPS position", Target: TargetCoordinates, Required: true},
		{Action: ActionReboot, Usage: "reboot <machine>", Summary: "Reboot a machine", Target: TargetMachine, Required: true},
		{Action: ActionShutdown, Usage: "shutdown <machine>", Summary: "Shut a machine down", Target: TargetMachine, Required: true},
		{Action: ActionServices, Usage: "services", Summary: "List the managed services"},
		{Action: ActionHelp, Usage: "help", Summary: "Command reference"},
		{Action: ActionFailed, Usage: "failed [machine]", Summary: "Failed systemd units", Target: TargetMachine},
		{Action: ActionHealth, Usage: "health", Summary: "Disk, load, memory, units and certificates"},
		{Action: ActionDocker, Usage: "docker", Summary: "How to use the docker commands"},
		{Action: ActionDockerPs, Usage: "docker ps", Summary: "Allowed containers and their state"},
		{Action: ActionDockerRestart, Usage: "docker restart <container>", Summary: "Restart an allowed container", Target: TargetContainer, Required: true},
		{Action: ActionDockerLogs, Usage: "docker logs <container>", Summary: "Last lines of a container's log", Target: TargetContainer, Required: true},
		{Action: ActionDigest, Usage: "digest", Summary: "Send the health digest now"},
	}
}
