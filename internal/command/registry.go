package command

// Handler is a function that handles a Command and returns a Response.
type Handler func(cmd Command) Response

// Registry maps Actions to their Handlers.
type Registry struct {
	handlers map[Action]Handler
}

// NewRegistry creates a new empty Registry.
func NewRegistry() *Registry {
	return &Registry{handlers: make(map[Action]Handler)}
}

// Register associates a Handler with an Action.
func (r *Registry) Register(action Action, handler Handler) {
	r.handlers[action] = handler
}

// Lookup retrieves the Handler for an Action. Returns false if not registered.
func (r *Registry) Lookup(action Action) (Handler, bool) {
	h, ok := r.handlers[action]
	return h, ok
}
