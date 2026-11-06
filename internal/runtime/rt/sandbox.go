package rt

// Sandbox confines a session's commands (and every process they start): writes only to the session's working
// directory, the sandbox's own temporary directory and AllowWrite; outbound connections only to AllowedDomains and the
// Unix sockets UnixSockets. Each runtime maps it onto its own sandbox (Claude Code's "sandbox" settings block, Codex's
// workspace-write sandbox).
type Sandbox struct {
	// Required makes the session fail at startup when the sandbox cannot start; otherwise the runtime may warn and run
	// commands unsandboxed.
	Required bool
	// AllowWrite are paths writable besides the working directory; DenyWrite are paths never writable, even inside
	// an allowed one; DenyRead are paths that cannot be read.
	AllowWrite, DenyWrite, DenyRead []string
	// AllowedDomains are the hosts commands may reach ("*.example.com" matches the subdomains).
	AllowedDomains []string
	// UnixSockets are the Unix sockets commands may connect to (macOS only: Linux cannot filter sockets by path).
	UnixSockets []string
	// AllowAllUnixSockets lets commands connect to any Unix socket; on Linux it is the only way to reach one.
	AllowAllUnixSockets bool
	// WeakerNetworkIsolation (macOS) lets commands reach the trustd service, which Go programs such as gh verify every
	// TLS certificate through. It opens a path out of the sandbox through that service.
	WeakerNetworkIsolation bool
}
