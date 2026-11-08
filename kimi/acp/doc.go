// Package acp implements the Agent Client Protocol (ACP) v1 in Go with zero
// third-party dependencies.
//
// # Wire format
//
// ACP uses JSON-RPC 2.0 over UTF-8, newline-delimited JSON frames on stdio:
// one request/response/notification per line, no embedded newlines. stdout is
// reserved for protocol frames; diagnostics belong on stderr. Request ids MUST
// be strings or integers, never null. Both peers may issue requests, so a
// [Conn] is symmetric and full duplex — an Agent handling session/prompt can
// call session/request_permission, fs/* or terminal/* back into the Client
// while the original request is still pending.
//
// # Layers
//
//   - acp: frame parsing, transports, the bidirectional Conn, typed schema/v1
//     structs and open unions (ContentBlock, SessionUpdate, capabilities).
//   - client: the Client side (initialize, session/new, session/load,
//     session/resume, session/list, session/close, session/delete,
//     session/fork, session/prompt, session/cancel, authenticate, logout),
//     subprocess launching, and local fs/terminal handlers.
//   - agent: the Agent side with a cancellable prompt context and an Emitter
//     for streaming session/update plus reverse calls.
//   - bridge: a transparent ACP proxy that remaps request ids so
//     $/cancel_request crosses the hop correctly.
//
// # Client -> Agent methods
//
//	initialize, authenticate, logout,
//	session/new, session/load, session/resume, session/fork, session/list,
//	session/close, session/delete, session/prompt, session/set_mode,
//	session/set_config_option
//	session/cancel (notification), $/cancel_request (notification)
//
// # Agent -> Client methods
//
//	session/request_permission, fs/read_text_file, fs/write_text_file,
//	terminal/create, terminal/output, terminal/wait_for_exit,
//	terminal/kill, terminal/release
//	session/update (notification)
//
// All file paths MUST be absolute; line numbers are 1-based; discriminator
// values use snake_case while object keys use camelCase. Unknown methods are
// answered with -32601 (methodNotFound) and unknown notifications are ignored,
// which is how ACP extensions stay compatible.
//
// # Kimi Code CLI specifics (measured 2026-11-08, kimi 2.1.1)
//
// `kimi acp` negotiates protocolVersion 1 and advertises loadSession,
// promptCapabilities.image/embeddedContext, mcpCapabilities.http/sse,
// sessionCapabilities list/resume/close/delete/fork/additionalDirectories and
// auth.logout. authMethods contains one terminal login method. Two deviations
// from the published schema shaped this implementation:
//
//   - authMethods[].env is an object ({}) rather than EnvVariable[], so
//     [AuthMethod.Env] is kept as json.RawMessage instead of a typed slice.
//   - sessionCapabilities.fork is advertised although schema/v1 does not
//     define a fork request; [SessionCapabilities] is an open map and
//     [client.Client.ForkSession] forwards raw params.
//
// When the CLI is not logged in, session/new returns -32000 "Authentication
// required"; session/list still works. Unknown methods return -32601 with a
// data.method field. See docs/probes/index.json for the raw evidence.
//
// # Verification boundary
//
// The library, the Go echo agent, the bridge and the CLI are covered by tests
// (including real subprocess round trips). Against the real `kimi acp` only
// read-only probes were run: initialize, session/list, a failed session/new,
// and an unknown method. No session/prompt was sent to a real Kimi session,
// so Kimi's model/tool streaming and permission behaviour are NOT verified by
// this project; the Go echo agent is the deterministic stand-in.
package acp
