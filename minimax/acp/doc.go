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
// # MiniMax Code CLI specifics
//
// MiniMax Code (npm `@minimax-ai/code`, CLI `mcode`) ships a native ACP v1
// stdio server behind `mcode acp`. It is the officially recommended editor
// integration, so this project does NOT re-implement an agent protocol for
// MiniMax: it drives the native server as a Client, or proxies it verbatim
// with [bridge]. `mcode acp --help` confirms the server command and its
// `--system-prompt*` / `login` subcommands; `mcode login` (or
// `mcode login --region global` / `mcode acp login --region <region>`)
// establishes the account session used by the native server.
//
// Because the vendor's capability declaration was NOT obtained in this
// environment (see below), every capability object here is kept open:
// [SessionCapabilities], [AgentAuthCapabilities], [AuthMethod.Env],
// [SessionUpdate] and [ContentBlock] preserve unknown vendor fields instead
// of decoding them into closed structs. In particular:
//
//   - [AuthMethod.Env] stays json.RawMessage: schema/v1 declares
//     EnvVariable[], but ACP agents in the wild also send an object.
//   - `session/fork` is advertised by some agents although schema/v1 defines
//     no request body; [client.Client.ForkSession] forwards raw params.
//   - MiniMax's ACP interface is documented as accepting text prompts while
//     not advertising image/audio/video inputs. [PromptCapabilities] is still
//     decoded generically — do not infer support from our own struct.
//
// Never hard-code a vendor capability list into this library. Run
// `minimax-acp doctor` to obtain the live `initialize` result for the
// installed CLI version and record it under docs/probes/.
//
// # Verification boundary
//
// The library, the Go echo agent, the bridge and the CLI are covered by tests
// (including real subprocess round trips). On this machine `mcode --version`
// reports 0.6.3 and `mcode acp --help` lists the native ACP server, but the
// read-only `initialize` handshake produced NO frame at all under the
// WorkBuddy sandbox (see docs/probes/). Therefore the real agentCapabilities,
// authMethods, model catalog, tool/permission behaviour and streamed output
// of `mcode acp` are NOT verified by this project. The Go echo agent and fake
// servers are deterministic stand-ins and must never be presented as proof of
// real MiniMax compatibility. No `session/prompt` was ever sent to a real
// MiniMax session.
package acp
