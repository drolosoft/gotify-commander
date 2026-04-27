package security

// Whitelist holds a set of allowed service names.
type Whitelist struct {
	allowed map[string]bool
}

// NewWhitelist creates a new Whitelist from the given map of service names.
func NewWhitelist(services map[string]bool) *Whitelist {
	return &Whitelist{allowed: services}
}

// IsAllowed reports whether the given name is in the whitelist.
func (w *Whitelist) IsAllowed(name string) bool {
	if name == "" {
		return false
	}
	return w.allowed[name]
}
