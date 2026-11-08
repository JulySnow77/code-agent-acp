package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tizerluo/zcode-open-bridge-go/internal/compat"
	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// doctor 是启动自检子命令 (PORTING_PLAN Phase 1 验收项)。
//
// 输出顺序即自检顺序: 解析 ZCODE_BIN → 推导/校验 provider 配置 → zcode --version
// → 注入凭证 → 协议探测 → session/create 计时 → 模型目录 → ZCODE_ACP_MODEL 校验。
func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("doctor", stderr)
	jsonOut := fs.Bool("json", false, "以 JSON 输出 (供脚本消费)")
	cwd := fs.String("cwd", "", "session/create 用的工作区路径 (默认当前目录)")
	noCreate := fs.Bool("no-create", false, "跳过 session/create 计时 (不建真实会话)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := newLogger(stderr)
	b := newBootstrap(log)

	rep := doctorReport{}
	prepErr := b.prepare()
	rep.Bin = b.report.Bin
	rep.ResolvedBin = b.report.ResolvedBin
	rep.Args = b.report.Args
	rep.ProviderConfig = b.report.ProviderConfig
	rep.ProviderSource = b.report.ProviderSource
	rep.Findings = append(rep.Findings, b.report.Findings...)
	rep.Injected = b.report.Injected
	rep.Passed = b.report.Passed
	rep.AppVersion = appVersion()

	if v, err := detectZcodeVersion(b.report.ResolvedBin, b.env); err != nil {
		rep.Findings = append(rep.Findings, compat.Finding{Level: "error", Msg: fmt.Sprintf("zcode --version 探测失败: %v", err)})
	} else {
		b.version = v
		rep.CLIVersion = v.String()
	}

	if prepErr == nil {
		be, err := b.start()
		if err != nil {
			rep.Findings = append(rep.Findings, compat.Finding{Level: "error", Msg: fmt.Sprintf("启动 zcode app-server 失败: %v", err)})
		} else {
			defer be.Close()
			// 等协议探测出结果 (后台跑, 冷启动约 20s), 拿实锤结论而非兜底值。
			be.WaitProbe(30 * time.Second)
			rep.ProtocolMode = string(be.Mode())
			rep.ProtocolConfirmed = be.Confirmed()
			rep.ProtocolVersion = be.ProtocolVersion
			if !*noCreate {
				runCreateProbe(be, b, *cwd, &rep, log)
			}
			if len(be.StderrTail(20)) > 0 {
				rep.StderrTail = be.StderrTail(20)
			}
		}
	} else {
		rep.Findings = append(rep.Findings, compat.Finding{Level: "error", Msg: prepErr.Error()})
	}

	rep.OK = !rep.hasError()
	if *jsonOut {
		return writeJSON(stdout, &rep)
	}
	writeHuman(stdout, &rep)
	if !rep.OK {
		return 1
	}
	return 0
}

func runCreateProbe(be *zcode.Backend, b *bootstrap, cwdOverride string, rep *doctorReport, log func(string, ...any)) {
	if cwdOverride == "" {
		if wd, err := os.Getwd(); err == nil {
			cwdOverride = wd
		} else {
			cwdOverride = "."
		}
	}
	params := map[string]any{
		"workspace": map[string]any{"workspacePath": cwdOverride, "workspaceKey": cwdOverride},
		"mode":      b.cfg.Mode,
	}
	rw := compat.Rewriter{Version: b.version, Timeout: b.timeouts}
	params, timeout, err := rw.Rewrite("session/create", params)
	if err != nil {
		rep.Findings = append(rep.Findings, compat.Finding{Level: "error", Msg: err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	resp, err := be.Request(ctx, "10000001", "session/create", params)
	rep.CreateElapsedMs = time.Since(start).Milliseconds()
	rep.CreateTimeoutS = int(timeout.Seconds())
	if err != nil {
		rep.Findings = append(rep.Findings, compat.Finding{Level: "error", Msg: fmt.Sprintf("session/create 失败 (等待 %.1fs): %v", time.Since(start).Seconds(), err)})
		if be.Dead() {
			rep.Findings = append(rep.Findings, compat.Finding{Level: "error", Msg: be.ExitDetail()})
			if tail := be.StderrTail(20); len(tail) > 0 {
				rep.StderrTail = tail
			}
		}
		return
	}
	result := resp.ResultMap()
	rep.SessionID = zcode.Str(zcode.Sub(result, "session"), "sessionId")
	if rep.SessionID == "" {
		rep.SessionID = zcode.Str(result, "sessionId")
	}
	if rep.SessionID == "" {
		rep.Findings = append(rep.Findings, compat.Finding{Level: "error", Msg: "session/create 未返回 sessionId"})
		return
	}
	if proto, ok := result["protocol"].(map[string]any); ok {
		rep.ProtocolName = zcode.Str(proto, "name")
		rep.ProtocolVersion = int(zcode.Num(proto, "version"))
		be.ProtocolVersion = rep.ProtocolVersion
	}
	catalog := zcode.ParseModelCatalog(result)
	rep.ModelCount = len(catalog.Entries)
	for i, e := range catalog.Entries {
		if i >= 12 {
			break
		}
		rep.Models = append(rep.Models, doctorModel{
			Ref: e.Ref.String(), Label: e.Label,
			ContextWindow: e.ContextWindow, Levels: e.Levels,
		})
	}
	if rep.ModelCount > len(rep.Models) {
		rep.ModelsTruncated = rep.ModelCount - len(rep.Models)
	}
	rep.Findings = append(rep.Findings, compat.Finding{Level: "info", Msg: fmt.Sprintf("session/create 耗时 %dms (超时上限 %ds)", rep.CreateElapsedMs, rep.CreateTimeoutS)})

	// ZCODE_ACP_MODEL 校验: 提前报「所选模型不在目录中」, 而不是等 turn 失败。
	if spec := strings.TrimSpace(b.cfg.Model); spec != "" {
		rep.RequestedModel = spec
		if ref, err := catalog.Resolve(spec); err != nil {
			rep.Findings = append(rep.Findings, compat.Finding{Level: "error", Msg: err.Error()})
		} else {
			rep.ResolvedModel = ref.String()
			rep.ResolvedReasoning = catalog.DefaultReasoningLevel(ref)
			rep.Findings = append(rep.Findings, compat.Finding{Level: "info", Msg: fmt.Sprintf("ZCODE_ACP_MODEL=%s 解析为 %s (reasoningLevel=%q)",
				spec, ref.String(), rep.ResolvedReasoning)})
		}
	}
	if catalog.Empty() {
		rep.Findings = append(rep.Findings, compat.Finding{Level: "warn", Msg: "模型目录为空: 独立 CLI 进程的目录里通常没有内置 GLM provider, " +
			"只有用户自建 provider (PORTING_PLAN §2.2 C11)。" +
			"请用 ZCODE_ACP_MODEL 显式指定, 或先调查 ZCODE_CREDENTIAL_SECRET 的传递。"})
	}
	log("session/create ok (sid=%s, %d 个模型)", rep.SessionID, rep.ModelCount)
}

type doctorModel struct {
	Ref           string   `json:"ref"`
	Label         string   `json:"label,omitempty"`
	ContextWindow int      `json:"contextWindow,omitempty"`
	Levels        []string `json:"levels,omitempty"`
}

type doctorReport struct {
	OK                bool              `json:"ok"`
	Bin               string            `json:"bin"`
	ResolvedBin       string            `json:"resolvedBin"`
	Args              []string          `json:"args,omitempty"`
	AppVersion        string            `json:"appVersion,omitempty"`
	CLIVersion        string            `json:"cliVersion,omitempty"`
	ProtocolMode      string            `json:"protocolMode,omitempty"`
	ProtocolConfirmed bool              `json:"protocolConfirmed"`
	ProtocolName      string            `json:"protocolName,omitempty"`
	ProtocolVersion   int               `json:"protocolVersion,omitempty"`
	ProviderConfig    string            `json:"providerConfig,omitempty"`
	ProviderSource    string            `json:"providerSource,omitempty"`
	CreateElapsedMs   int64             `json:"createElapsedMs,omitempty"`
	CreateTimeoutS    int               `json:"createTimeoutS,omitempty"`
	SessionID         string            `json:"sessionId,omitempty"`
	ModelCount        int               `json:"modelCount,omitempty"`
	ModelsTruncated   int               `json:"modelsTruncated,omitempty"`
	Models            []doctorModel     `json:"models,omitempty"`
	RequestedModel    string            `json:"requestedModel,omitempty"`
	ResolvedModel     string            `json:"resolvedModel,omitempty"`
	ResolvedReasoning string            `json:"resolvedReasoning,omitempty"`
	Injected          map[string]string `json:"injected,omitempty"`
	Passed            map[string]string `json:"passed,omitempty"`
	StderrTail        []string          `json:"stderrTail,omitempty"`
	Findings          []compat.Finding  `json:"findings,omitempty"`
}

func (r *doctorReport) hasError() bool {
	for _, f := range r.Findings {
		if f.Level == "error" {
			return true
		}
	}
	return false
}

func writeHuman(w io.Writer, r *doctorReport) {
	status := "✅ 全绿"
	if !r.OK {
		status = "❌ 有问题"
	}
	fmt.Fprintf(w, "\n=== zcode-acp-bridge doctor (%s) ===\n\n", status)

	section(w, "可执行文件")
	kv(w, "ZCODE_BIN", r.Bin)
	kv(w, "解析后", r.ResolvedBin)
	kv(w, "启动参数", strings.Join(r.Args, " "))
	kv(w, "App 版本", orDash(r.AppVersion))
	kv(w, "CLI 版本", orDash(r.CLIVersion))

	section(w, "provider 配置 (C1)")
	kv(w, "文件", orDash(r.ProviderConfig))
	kv(w, "来源", map[string]string{"env": "环境变量", "derived": "按 ZCODE_BIN 推导"}[r.ProviderSource])

	section(w, "协议")
	if r.ProtocolMode != "" {
		mode := map[string]string{"v16": "0.16+ 新协议", "legacy": "≤0.15 旧协议"}[r.ProtocolMode]
		if !r.ProtocolConfirmed {
			mode += " (超时兜底, 未确认)"
		}
		kv(w, "模式", mode)
	}
	if r.ProtocolName != "" {
		kv(w, "握手", fmt.Sprintf("%s v%d", r.ProtocolName, r.ProtocolVersion))
	}

	if r.CreateTimeoutS > 0 || r.SessionID != "" {
		section(w, "session/create (C2)")
		if r.SessionID != "" {
			kv(w, "sessionId", r.SessionID)
			kv(w, "耗时", fmt.Sprintf("%dms (上限 %ds)", r.CreateElapsedMs, r.CreateTimeoutS))
		} else {
			kv(w, "耗时", fmt.Sprintf("%dms 后失败 (上限 %ds)", r.CreateElapsedMs, r.CreateTimeoutS))
		}
	}

	section(w, "模型目录 (C3)")
	if r.ModelCount == 0 {
		fmt.Fprintln(w, "  (空)")
	} else {
		for _, m := range r.Models {
			extra := ""
			if m.Label != "" && m.Label != m.Ref {
				extra = "  " + m.Label
			}
			if len(m.Levels) > 0 {
				extra += fmt.Sprintf("  levels=%v", m.Levels)
			}
			fmt.Fprintf(w, "  • %s%s\n", m.Ref, extra)
		}
		if r.ModelsTruncated > 0 {
			fmt.Fprintf(w, "  … 另有 %d 个\n", r.ModelsTruncated)
		}
	}
	if r.RequestedModel != "" {
		kv(w, "ZCODE_ACP_MODEL", r.RequestedModel)
		kv(w, "解析结果", orDash(r.ResolvedModel))
		if r.ResolvedReasoning != "" {
			kv(w, "缺省 reasoningLevel", r.ResolvedReasoning)
		}
	}

	section(w, "注入的环境 (脱敏)")
	printMap(w, r.Injected)
	fmt.Fprintln(w)
	section(w, "透传的 ZCode 环境变量 (脱敏)")
	printMap(w, r.Passed)

	if len(r.StderrTail) > 0 {
		fmt.Fprintln(w)
		section(w, "子进程 stderr 末尾")
		for _, ln := range r.StderrTail {
			fmt.Fprintf(w, "  %s\n", ln)
		}
	}

	if len(r.Findings) > 0 {
		fmt.Fprintln(w)
		section(w, "诊断")
		order := map[string]int{"error": 0, "warn": 1, "info": 2}
		fs := make([]compat.Finding, len(r.Findings))
		copy(fs, r.Findings)
		sort.SliceStable(fs, func(i, j int) bool { return order[fs[i].Level] < order[fs[j].Level] })
		for _, f := range fs {
			mark := map[string]string{"error": "❌", "warn": "⚠", "info": "ℹ"}[f.Level]
			fmt.Fprintf(w, "  %s %s\n", mark, f.Msg)
		}
	}
	fmt.Fprintln(w)
}

func section(w io.Writer, name string) {
	fmt.Fprintf(w, "── %s ──\n", name)
}

func kv(w io.Writer, k, v string) {
	if v == "" {
		v = "-"
	}
	fmt.Fprintf(w, "  %-22s %s\n", k, v)
}

func printMap(w io.Writer, m map[string]string) {
	if len(m) == 0 {
		fmt.Fprintln(w, "  (无)")
		return
	}
	for _, k := range compat.SortedKeys(m) {
		fmt.Fprintf(w, "  %-34s %s\n", k, m[k])
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writeJSON(w io.Writer, r *doctorReport) int {
	b, err := jsonMarshalIndent(r)
	if err != nil {
		fmt.Fprintf(w, "{\"ok\":false,\"error\":%q}\n", err.Error())
		return 1
	}
	fmt.Fprintln(w, string(b))
	if !r.OK {
		return 1
	}
	return 0
}
