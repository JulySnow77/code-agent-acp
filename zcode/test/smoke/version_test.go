//go:build real

package smoke

import (
	"os/exec"
	"strings"
	"testing"
)

// TestSmokeBridgeVersion 是本包里唯一不连真机的用例, 作用是**每次跑真机之前先
// 把「这次用的是哪个二进制、什么版本」摆在屏幕上**。
//
// 为什么需要它: 真机用例各自 `go build` 一份桥 (或用 ZCODE_ACP_BRIDGE_BIN 指向
// 已安装的那份), 所以「正在被测的到底是哪份代码」在跑之前是没有答案的。出了问
// 题回看日志, 看到的是行为不是版本 —— 而这个项目正处在「工作树里的改动和已
// 安装的二进制常常不是同一份」的状态 (smoke 默认现编, 日常入口用已安装的),
// 于是「我改了怎么没生效」这类判断只能靠猜。bridgeBinary 的选择逻辑正是
// 这个不确定性的所在, 所以版本号必须由它自己报。
//
// 单独执行 (不碰真机, 秒级):
//
//	go test -count=1 -tags=real ./test/smoke -run '^TestSmokeBridgeVersion$' -v
func TestSmokeBridgeVersion(t *testing.T) {
	bin := bridgeBinary(t)

	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("运行 %s version 失败: %v\n%s", bin, err, out)
	}
	report := strings.TrimRight(string(out), "\n")
	if report == "" {
		t.Fatalf("%s version 没有输出", bin)
	}

	// 报告里的第一行就是版本号本身, 原样打出来最有用 —— 完整信息 (go 版本、
	// upstream 基线、GOOS/GOARCH) 跟在后面。
	t.Logf("本次真机验证使用的桥:\n%s", indent(report))
	t.Logf("  二进制路径: %s", bin)

	// 版本号不能是空串或只剩前缀: `zcode-acp-bridge` 后面必须跟一个版本。
	// buildVersion 的回退链 (ldflags → build info → 常量) 任何一环坏掉都可能
	// 只剩下程序名, 那时候这行输出看着正常, 实际没版本可言。
	first := strings.SplitN(report, "\n", 2)[0]
	fields := strings.Fields(first)
	if len(fields) < 2 || fields[0] != "zcode-acp-bridge" {
		t.Errorf("version 首行应为「zcode-acp-bridge <版本>」, 实际 %q", first)
	} else if fields[1] == "" {
		t.Errorf("version 首行没有版本号: %q", first)
	}
}
