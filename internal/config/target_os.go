package config

import "fmt"

// The operating systems an SSH target can declare in ssh_targets.<name>.os.
const (
	// OSLinux is the default: free, ss, apt, journalctl and systemctl.
	OSLinux = "linux"

	// OSDarwin is macOS: vm_stat, lsof, netstat, softwareupdate, log show
	// and launchctl.
	OSDarwin = "darwin"
)

// validateTargetOS checks the os of every SSH target and writes OSLinux into
// the ones that leave it out, so the rest of the plugin sees one of the two
// values. A typo such as "macos" is refused instead of falling back to the
// linux commands, which would fail on the Mac one by one.
func validateTargetOS(cfg *Config) error {
	for name, target := range cfg.SSHTargets {
		switch target.OS {
		case "":
			target.OS = OSLinux
			cfg.SSHTargets[name] = target
		case OSLinux, OSDarwin:
		default:
			return fmt.Errorf("ssh_targets.%s.os must be %q or %q, got %q", name, OSLinux, OSDarwin, target.OS)
		}
	}
	return nil
}
