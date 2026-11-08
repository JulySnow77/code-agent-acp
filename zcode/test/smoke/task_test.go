//go:build real

package smoke

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 这两个开关就是本用例的全部入参 —— 在哪个目录干活、干什么活。
//
// go test 会把 -args 之后的原样传给测试二进制, 所以命令行写法是:
//
//	go test -count=1 -tags=real ./test/smoke -run '^TestSmokeRealTask$' -v -timeout 30m \
//	  -args -task.dir=/path/to/project -task.prompt="给 main.go 加上单元测试"
//
// 懒得敲命令行就写进项目根的 env: ZCODE_SMOKE_PROJECT_DIR / ZCODE_SMOKE_PROMPT。
// 命令行优先于 env —— 临时跑一次别的任务不用改配置文件。
//
// 为什么叫 -task.dir 而不是 -dir: go test 自己吃掉一批短开关 (-run/-v/-timeout...),
// 带前缀的名字不会跟它们、也不会跟将来加的开关撞车。
var (
	flagTaskDir    = flag.String("task.dir", "", "要执行任务的项目目录 (绝对路径)")
	flagTaskPrompt = flag.String("task.prompt", "", "交给模型的任务描述")
)

// TestSmokeRealTask 由两个参数驱动, 在真实 ZCode 上跑一个任意任务:
// 指定一个项目目录 + 一句 prompt, 让模型真的动手, 最后报告它做了什么。
//
// 不传参数就 Skip —— 它不像 TestSmokeRealBuildTask 那样自带 fixture, 因为它的
// 用途本来就是「拿我自己的项目跑我自己的任务」。
//
// 断言只有一条硬性判据: turn 必须以 end_turn 正常收尾。此外全是报告 —— 模型调
// 了哪些工具、说了什么、工作区多了/改了哪些文件。真正验证了什么由你自己看着
// 这些输出判断, 装置不替你假装有断言。
//
// ⚠ 这不是演练: 模型会在你指定的目录里真的创建/修改/删除文件。建议挑一个
// git 干净的目录, 跑之前这儿会先把未提交改动列出来给你看。
func TestSmokeRealTask(t *testing.T) {
	if os.Getenv("ZCODE_BIN") == "" {
		t.Skip("ZCODE_BIN 未设置 (真机任务需要)")
	}

	dir := firstSet(*flagTaskDir, taskDir())
	prompt := firstSet(*flagTaskPrompt, taskPromptFallback())
	if dir == "" || prompt == "" {
		t.Skip("缺少参数 —— 本用例需要「项目目录 + prompt」两项, 任选一种给法:\n" +
			"  go test -count=1 -tags=real ./test/smoke -run '^TestSmokeRealTask$' -v -timeout 30m \\\n" +
			"    -args -task.dir=/path/to/project -task.prompt=\"要做什么\"\n" +
			"  或写进项目根的 env: ZCODE_SMOKE_PROJECT_DIR / ZCODE_SMOKE_PROMPT")
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("解析项目目录 %q: %v", dir, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("项目目录不可用: %v", err)
	}
	if !st.IsDir() {
		t.Fatalf("%s 不是目录", abs)
	}

	t.Logf("任务目标目录: %s", abs)
	t.Logf("任务 prompt: %s", prompt)
	if dirty := gitStatus(t, abs); dirty != "" {
		t.Logf("⚠ 跑之前该目录已有未提交改动 (任务造成的变更会混在里面):\n%s", indent(dirty))
	}

	res := runTask(t, TaskOptions{Workspace: abs, Prompt: prompt})

	if res.PromptError != nil {
		t.Fatalf("session/prompt 返回错误: %v", res.PromptError)
	}
	if len(res.ToolCalls) == 0 {
		t.Errorf("整个 turn 没有调用任何工具 —— prompt 可能被当成纯问答回答了, " +
			"任务没真的执行")
	}
	t.Logf("工具调用 (%d): %s", len(res.ToolCalls), strings.Join(res.ToolCalls, " → "))
	if text := strings.TrimSpace(res.FinalText); text != "" {
		t.Logf("模型回复:\n%s", indent(truncate(text, 4000)))
	}
	if res.PermAnswered == 0 {
		t.Logf("ℹ 没有发生工具授权请求 (zcode 可能把工具判成了只读/低风险)")
	}

	// AskUserQuestion 链路: **不能只凭 end_turn 就算过**。桥把整组问题 cancel
	// 掉、模型自己改问一遍, turn 照样正常结束; 所以只要本轮走过 elicitation,
	// 就把「我们答的」与「zcode 实收的」逐条核对 (交互模式还应含多选)。
	if len(res.AskActions) > 0 {
		assertAskReachedZcode(t, res.WireLog, res.AskAnswers, res.AskActions)
	} else {
		t.Logf("ℹ 本轮没有 elicitation 提问 (未走 AskUserQuestion), 跳过作答核对")
	}

	// 任务做了什么, 由工作区说了算 —— 不采信模型自述。
	if changes := gitStatus(t, abs); changes != "" {
		t.Logf("任务造成的工作区变更:\n%s", indent(changes))
	} else {
		t.Logf("ℹ 工作区无 git 变更记录 (目录可能不受 git 管理, 或文件没动)")
	}
	if res.StopReason != "end_turn" {
		t.Errorf("stopReason = %s, want end_turn —— 任务没有正常结束", res.StopReason)
	}
	t.Logf("线路日志: %s", res.WireLog)
}

// gitStatus 列出目录里的未提交改动; 目录不在 git 仓库里则返回空串 —— 拿不到
// 清单只是少一份旁证, 不该让用例失败。
func gitStatus(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// firstSet 取第一个非空值 —— 命令行开关优先, 其次 env。
func firstSet(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("%s\n… (截断, 共 %d 字符)", s[:n], len(s))
}

// assertAskReachedZcode 从线材日志里取桥对 interaction/requestUserInput 的应答,
// 核对「我们答的」与「zcode 实际收到的」一致。
//
// 为什么值得单独断言: end_turn 只说明 turn 收尾正常。降级路径遇多选会整组 cancel,
// 随后模型改问单题再自己走完 —— 全程 end_turn, 但人的第一次作答根本没到 zcode。
func assertAskReachedZcode(t *testing.T, wirePath string, clientContent map[string]any, actions []string) {
	t.Helper()
	if len(actions) > 0 {
		t.Logf("本轮 elicitation 应答 action 序列: %v", actions)
		for _, a := range actions {
			if a == "accept" {
				goto hasAccept
			}
		}
		t.Errorf("本轮 %d 次 elicitation 应答全是 %v, 没有一次 accept —— "+
			"人的作答没有以 accept 形式到达桥", len(actions), actions)
		return
	}
hasAccept:
	if clientContent == nil {
		t.Errorf("有 accept 应答但没记录到答案内容, 无法核对")
		return
	}
	raw, err := os.ReadFile(wirePath)
	if err != nil {
		t.Errorf("读线材日志失败: %v", err)
		return
	}
	var zcodeReqID string
	var reply map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			Dir  string         `json:"dir"`
			Kind string         `json:"kind"`
			Msg  map[string]any `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		switch {
		case rec.Dir == "zcode→bridge" && rec.Kind == "interaction/requestUserInput":
			zcodeReqID = fmt.Sprint(rec.Msg["ID"])
		case rec.Dir == "bridge→zcode" && rec.Kind == "reply" && zcodeReqID != "" &&
			fmt.Sprint(rec.Msg["id"]) == zcodeReqID:
			reply, _ = rec.Msg["result"].(map[string]any)
		}
	}
	if reply == nil {
		t.Errorf("线材日志里没有对 interaction/requestUserInput (%s) 的应答", zcodeReqID)
		return
	}
	bts, _ := json.Marshal(reply)
	t.Logf("zcode 实际收到的 AskUserQuestion 应答: %s", bts)
	if action, _ := reply["action"].(string); action != "accept" {
		t.Errorf("应答 action = %v, want accept: %s", action, bts)
		return
	}
	content, _ := reply["content"].(map[string]any)
	answers, _ := content["answers"].(map[string]any)
	if len(answers) == 0 {
		t.Errorf("content.answers 为空, 回答没回灌: %s", bts)
		return
	}
	assertClientAnswersReachedZcode(t, clientContent, answers, bts)
}
