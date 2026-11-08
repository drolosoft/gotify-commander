package command

// Action represents a parsed command action.
type Action string

const (
	ActionRestart     Action = "restart"
	ActionStop        Action = "stop"
	ActionStart       Action = "start"
	ActionStatus      Action = "status"
	ActionReboot      Action = "reboot"
	ActionFree        Action = "free"
	ActionDf          Action = "df"
	ActionLogs        Action = "logs"
	ActionUptime      Action = "uptime"
	ActionWho         Action = "who"
	ActionPing        Action = "ping"
	ActionServices    Action = "services"
	ActionHelp        Action = "help"
	ActionShutdown    Action = "shutdown"
	ActionTop         Action = "top"
	ActionPorts       Action = "ports"
	ActionIp          Action = "ip"
	ActionUpdates     Action = "updates"
	ActionCerts       Action = "certs"
	ActionConnections Action = "connections"
	ActionTraffic     Action = "traffic"
	ActionAnalytics   Action = "analytics"
	ActionLocate      Action = "locate"

	// ActionConfirm answers a confirmation request: "<keyword> 4821", with
	// the keyword of the configuration ("confirm" by default), runs the
	// command that is waiting for that code. The code travels in
	// Command.Target. It has no handler in the registry; the commander
	// takes it before the lookup.
	ActionConfirm Action = "confirm"
)

// Operations actions, registered by RegisterExtras. docker gets one action
// per subcommand, so that only docker-restart is dangerous; ActionDocker
// itself only answers the usage.
const (
	ActionFailed        Action = "failed"
	ActionHealth        Action = "health"
	ActionDigest        Action = "digest"
	ActionDocker        Action = "docker"
	ActionDockerPs      Action = "docker-ps"
	ActionDockerRestart Action = "docker-restart"
	ActionDockerLogs    Action = "docker-logs"
)

// Command holds a parsed user command.
type Command struct {
	Action Action
	Target string
	Raw    string
}

// Response is the result of handling a Command.
type Response struct {
	Title    string
	Message  string
	Priority int
	Markdown bool // if true, message is rendered as markdown
}
