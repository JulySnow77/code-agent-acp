package acp

// ACP v1 method names. Methods are grouped by the side that *serves* them, but
// either side of a Conn may register a handler for any method (a transparent
// proxy does exactly that).
const (
	// Client -> Agent
	MethodInitialize          = "initialize"
	MethodAuthenticate        = "authenticate"
	MethodLogout              = "logout"
	MethodSessionNew          = "session/new"
	MethodSessionLoad         = "session/load"
	MethodSessionResume       = "session/resume"
	MethodSessionFork         = "session/fork" // Kimi extension (not in schema/v1 core)
	MethodSessionList         = "session/list"
	MethodSessionClose        = "session/close"
	MethodSessionDelete       = "session/delete"
	MethodSessionPrompt       = "session/prompt"
	MethodSessionSetMode      = "session/set_mode"
	MethodSessionSetConfigOpt = "session/set_config_option"

	// Notifications (Client -> Agent)
	MethodSessionCancel = "session/cancel"
	MethodCancelRequest = "$/cancel_request"

	// Agent -> Client methods
	MethodSessionRequestPermission = "session/request_permission"
	MethodFSReadTextFile           = "fs/read_text_file"
	MethodFSWriteTextFile          = "fs/write_text_file"
	MethodTerminalCreate           = "terminal/create"
	MethodTerminalOutput           = "terminal/output"
	MethodTerminalWaitForExit      = "terminal/wait_for_exit"
	MethodTerminalKill             = "terminal/kill"
	MethodTerminalRelease          = "terminal/release"

	// Agent -> Client notification
	MethodSessionUpdate = "session/update"
)

// Session update discriminators (schema/v1 SessionUpdate).
const (
	UpdateUserMessageChunk       = "user_message_chunk"
	UpdateAgentMessageChunk      = "agent_message_chunk"
	UpdateAgentThoughtChunk      = "agent_thought_chunk"
	UpdateToolCall               = "tool_call"
	UpdateToolCallUpdate         = "tool_call_update"
	UpdatePlan                   = "plan"
	UpdateAvailableCommands      = "available_commands_update"
	UpdateCurrentMode            = "current_mode_update"
	UpdateConfigOption           = "config_option_update"
	UpdateSessionInfo            = "session_info_update"
	UpdateUsage                  = "usage_update"
	UpdateNotice                 = "notice"
	UpdateCompaction             = "compaction_update"
	UpdateCompactionSummaryChunk = "compaction_summary_chunk"
)

// StopReason values (schema/v1 StopReason).
const (
	StopEndTurn         = "end_turn"
	StopMaxTokens       = "max_tokens"
	StopMaxTurnRequests = "max_turn_requests"
	StopRefusal         = "refusal"
	StopCancelled       = "cancelled"
)

// Tool kinds and statuses (schema/v1 ToolKind / ToolCallStatus).
const (
	ToolKindRead       = "read"
	ToolKindEdit       = "edit"
	ToolKindDelete     = "delete"
	ToolKindMove       = "move"
	ToolKindSearch     = "search"
	ToolKindExecute    = "execute"
	ToolKindThink      = "think"
	ToolKindFetch      = "fetch"
	ToolKindSwitchMode = "switch_mode"
	ToolKindOther      = "other"

	ToolStatusPending    = "pending"
	ToolStatusInProgress = "in_progress"
	ToolStatusCompleted  = "completed"
	ToolStatusFailed     = "failed"
)

// Permission option kinds.
const (
	PermissionAllowOnce    = "allow_once"
	PermissionAllowAlways  = "allow_always"
	PermissionRejectOnce   = "reject_once"
	PermissionRejectAlways = "reject_always"
)

// Permission outcomes.
const (
	OutcomeCancelled = "cancelled"
	OutcomeSelected  = "selected"
)

// ProtocolVersionV1 is the integer negotiated by initialize in ACP v1.
const ProtocolVersionV1 = 1
