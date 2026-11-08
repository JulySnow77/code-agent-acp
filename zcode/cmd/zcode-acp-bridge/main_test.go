package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildVersion 必须始终报出**项目声明的版本号**, 而不是 Go 的伪版本。
//
// 这条曾经是坏的: 旧实现在 bridgeVersion 保持默认值时去读 build info 的
// info.Main.Version, 而 git 检出里那总是个 v0.0.0-<时间>-<commit>+dirty
// (非空、也不是 "(devel)"), 于是伪版本永远盖住 0.1.0-dev —— 跑起来看着像项目
// 根本没有版本号。伪版本里的 commit 已经在 version 输出里单独打 vcs.revision,
// 顶掉声明版本只会让人误读。
func TestBuildVersionPrefersDeclaredOverPseudo(t *testing.T) {
	orig := bridgeVersion
	t.Cleanup(func() { bridgeVersion = orig })

	// 未注入 (本地构建): 报声明版本。
	bridgeVersion = defaultBridgeVersion
	if got := buildVersion(); got != defaultBridgeVersion {
		t.Errorf("本地构建的版本 = %q, want %q (不能被 build info 的伪版本顶掉)",
			got, defaultBridgeVersion)
	}

	// 注入过 (发布/安装构建): 以注入值为准 —— goreleaser 的 {{.Version}} 走这条。
	bridgeVersion = "0.1.0-dev+t03-1fe8909-wip"
	if got := buildVersion(); got != "0.1.0-dev+t03-1fe8909-wip" {
		t.Errorf("注入版本的版本 = %q, want 注入值", got)
	}
}

// TestCmdVersionPrintsProjectVersion: version 子命令的首行必须带版本号。
//
// 这条直接对应用户看到的现象: 「本项目的版本号没有显示」。
func TestCmdVersionPrintsProjectVersion(t *testing.T) {
	orig := bridgeVersion
	t.Cleanup(func() { bridgeVersion = orig })
	bridgeVersion = defaultBridgeVersion

	var out bytes.Buffer
	if code := cmdVersion(&out); code != 0 {
		t.Fatalf("cmdVersion 退出码 = %d, want 0", code)
	}
	report := out.String()

	first := strings.SplitN(report, "\n", 2)[0]
	if !strings.Contains(first, defaultBridgeVersion) {
		t.Errorf("version 首行应含项目版本号 %q, 实际 %q", defaultBridgeVersion, first)
	}
	// 提交号仍要能查到: 换到 revision 行去找, 而不是挤占版本号的位置。
	if !strings.Contains(report, "upstream:") {
		t.Errorf("version 应保留 upstream 基线信息:\n%s", report)
	}
}

// TestBuiltBinaryReportsProjectVersion 用**真实产物**验版本号。
//
// 为什么不能只在进程内调 buildVersion: 这个 bug 只在 go build 出的二进制里出现。
// `go test` 下 debug.ReadBuildInfo 的 Main.Version 是 "(devel)", 于是伪版本那条
// 分支根本走不到 —— 进程内断言在旧实现下照样全绿, 钉不住任何东西。
// 真实二进制 (go build, git 检出) 才拿到 v0.0.0-<时间>-<commit>+dirty。
//
// 这就是「测试全绿、用户看到版本号是伪版本」能同时成立的原因。
func TestBuiltBinaryReportsProjectVersion(t *testing.T) {
	// go test 的 cwd 是**包目录** (cmd/zcode-acp-bridge), .git 在两级之上 ——
	// 直接 stat ".git" 会永远判成「不在检出里」而静默 Skip, 看着像通过。
	if _, err := os.Stat(filepath.Join("..", "..", ".git")); err != nil {
		t.Skip("不在 git 检出里, 拿不到 build info 伪版本, 本用例的前提不成立")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("PATH 里没有 go")
	}

	bin := filepath.Join(t.TempDir(), "zcode-acp-bridge")
	build := exec.Command(goBin, "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build 失败: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s version 失败: %v\n%s", bin, err, out)
	}

	first := strings.SplitN(string(out), "\n", 2)[0]
	if !strings.Contains(first, defaultBridgeVersion) {
		t.Errorf("真实二进制的版本首行 = %q, want 含 %q", first, defaultBridgeVersion)
	}
	// 伪版本是 git 推导的时间戳+commit, 不是项目声明的版本号。
	if strings.Contains(first, "v0.0.0-") {
		t.Errorf("版本首行被 build info 的伪版本顶掉了: %q "+
			"(它随每次构建变化, 拿来当版本号会让人误以为项目没有版本)", first)
	}
}
