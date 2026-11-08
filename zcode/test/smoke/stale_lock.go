//go:build real

package smoke

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// keepStaleLocksVar=1 时跳过孤儿锁清理 (想自己手动清时用)。
const keepStaleLocksVar = "ZCODE_SMOKE_KEEP_STALE_LOCKS"

// 真机用例跑完会 kill app-server, zcode 来不及清它的插件 seeding 锁, 于是
// ~/.zcode/cli/plugins/cache/<市场>/<插件>/<版本>.seed-lock 里留下一批孤儿
// (owner.json 记着已经死掉的 pid)。下一次启动 zcode 想清理它们, 却撞上自己
// 的批量删除保护:
//
//	[safe-delete][SAFE_DELETE_BULK_CONFIRM_REQUIRED]
//	{"count":50,"threshold":50,"scope":"turn","targets":[".../node-repl-host/0.6.0.seed-lock"]}
//
// 于是 session/new 直接 -32603 —— **用例没跑, 报的还不是真正的原因**。
// 首轮 BuildTask 之所以能过, 只是因为当时磁盘是干净的。
//
// 所以在 TestMain 里、起后端之前, 把 pid 已死的孤儿锁提前收走。
//
// 安全边界:
//   - 只认 cache 下三级、名字以 .seed-lock 结尾的**目录**;
//   - 只读 owner.json 的 pid, pid 仍存活的一律不动 (那说明有活的 zcode 在用);
//   - 读不出 pid 的一律不动 (宁可漏, 不可错杀正在创建的锁);
//   - 只 os.Rename 到 ~/.Trash (可逆), 不 RemoveAll;
//   - 任何一步出错只记日志, 不阻断测试。
type staleLockReport struct {
	// Root 是扫描的根目录。
	Root string
	// Removed 是被收走的孤儿锁 (相对 Root)。
	Removed []string
	// Kept 是判断为「还活着」或读不出 pid 而未动的锁。
	Kept []string
	// Skipped 为 true 表示被 keepStaleLocksVar 关掉了。
	Skipped bool
	// Err 是整个过程的错误摘要, 非空只影响日志不影响测试。
	Err string
}

// cleanStaleLocks 收走 pid 已死的插件 seed-lock。
func cleanStaleLocks() staleLockReport {
	var rep staleLockReport
	home, err := os.UserHomeDir()
	if err != nil {
		rep.Err = fmt.Sprintf("取 HOME 失败: %v", err)
		return rep
	}
	rep.Root = filepath.Join(home, ".zcode", "cli", "plugins", "cache")
	if os.Getenv(keepStaleLocksVar) == "1" {
		rep.Skipped = true
		return rep
	}
	if _, err := os.Stat(rep.Root); err != nil {
		return rep // 没这个目录就没什么可清的
	}

	matches, err := filepath.Glob(filepath.Join(rep.Root, "*", "*", "*.seed-lock"))
	if err != nil {
		rep.Err = fmt.Sprintf("扫描失败: %v", err)
		return rep
	}
	sort.Strings(matches)

	for _, lock := range matches {
		rel, _ := filepath.Rel(rep.Root, lock)
		if st, err := os.Stat(lock); err != nil || !st.IsDir() {
			rep.Kept = append(rep.Kept, rel+" (不是目录)")
			continue
		}
		pid, ok := readLockOwnerPID(lock)
		if !ok {
			// 读不到 pid: 可能是正被别的 zcode 创建中的锁, 不碰。
			rep.Kept = append(rep.Kept, rel+" (读不到 pid)")
			continue
		}
		if pidAlive(pid) {
			rep.Kept = append(rep.Kept, fmt.Sprintf("%s (pid=%d 存活)", rel, pid))
			continue
		}
		dest := filepath.Join(home, ".Trash",
			filepath.Base(lock)+"."+time.Now().Format("20060102-150405.000"))
		if err := os.Rename(lock, dest); err != nil {
			rep.Err = fmt.Sprintf("移走 %s 失败: %v", rel, err)
			rep.Kept = append(rep.Kept, rel+" (移走失败)")
			continue
		}
		rep.Removed = append(rep.Removed, fmt.Sprintf("%s (pid=%d 已死)", rel, pid))
	}
	return rep
}

// readLockOwnerPID 从锁目录的 owner.json 里取持有者 pid。
func readLockOwnerPID(lock string) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(lock, "owner.json"))
	if err != nil {
		return 0, false
	}
	var owner struct {
		PID json.Number `json:"pid"`
	}
	if err := json.Unmarshal(raw, &owner); err != nil || owner.PID == "" {
		return 0, false
	}
	pid, err := owner.PID.Int64()
	if err != nil || pid <= 0 {
		return 0, false
	}
	return int(pid), true
}

// pidAlive 用 signal 0 探活 —— 只探不杀。
func pidAlive(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}

// Report 生成清理摘要, 供 TestMain 打印。
func (r staleLockReport) Report() string {
	var b strings.Builder
	switch {
	case r.Skipped:
		fmt.Fprintf(&b, "smoke 环境: 跳过孤儿锁清理 (%s=1)\n", keepStaleLocksVar)
		return b.String()
	case len(r.Removed) == 0 && len(r.Kept) == 0 && r.Err == "":
		return "" // 没有锁, 不必刷屏
	}
	fmt.Fprintf(&b, "smoke 环境: 清理 zcode 孤儿 seed-lock (上次跑完 kill 后端留下的)\n")
	for _, s := range r.Removed {
		fmt.Fprintf(&b, "    → 已移入废纸篓: %s\n", s)
	}
	for _, s := range r.Kept {
		fmt.Fprintf(&b, "    - 未动: %s\n", s)
	}
	if r.Err != "" {
		fmt.Fprintf(&b, "    ⚠ %s\n", r.Err)
	}
	return b.String()
}
