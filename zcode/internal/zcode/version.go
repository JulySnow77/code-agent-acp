package zcode

import (
	"fmt"
	"strconv"
	"strings"
)

// Version 是语义化版本 (major.minor.patch), 用于兼容层的版本判定。
// CLI 版本号不可全信: App 3.12.3 与更早的构建报同一个 0.16.5 而方法集不同
// (上游 2d449ff 实测), 故版本号只用于「引入版本」型规则, 能力探测优先。
type Version struct {
	Major, Minor, Patch int
}

// ParseVersion 解析 "0.16.5" 这类版本串。容忍前缀噪声 (如 "zcode 0.16.5")
// 与后缀 (如 "0.16.5-beta")。
func ParseVersion(s string) (Version, error) {
	s = strings.TrimSpace(s)
	// 取第一个形如 数字.数字 的片段
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			if s[i] == '.' {
				continue
			}
			s = s[:i]
			break
		}
	}
	if start > 0 {
		s = s[start:]
	}
	parts := strings.Split(s, ".")
	v := Version{}
	for i := 0; i < 3; i++ {
		if i >= len(parts) {
			break
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			if i == 0 {
				return Version{}, fmt.Errorf("无法解析版本号 %q", s)
			}
			break
		}
		switch i {
		case 0:
			v.Major = n
		case 1:
			v.Minor = n
		case 2:
			v.Patch = n
		}
	}
	return v, nil
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Compare 返回 -1/0/1。
func (v Version) Compare(o Version) int {
	for _, pair := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}

// AtLeast 判定 v >= o。
func (v Version) AtLeast(o Version) bool { return v.Compare(o) >= 0 }

// Zero 报告版本号是否为空 (0.0.0), 即探测失败。
func (v Version) Zero() bool { return v.Major == 0 && v.Minor == 0 && v.Patch == 0 }

// ProtocolMode 是服务端协议模式。0.16 起 zcode 的 app-server 换了 zod 校验层,
// 拒收带 jsonrpc 键的信封, 并新增 server→client 反向调用; 两侧行为差异足以
// 让「无反向调用应答」的客户端永久挂起。
type ProtocolMode string

const (
	// ProtocolLegacy 表示 ≤0.15 旧协议服务端。
	ProtocolLegacy ProtocolMode = "legacy"
	// ProtocolV16 表示 0.16+ 新协议服务端。
	ProtocolV16 ProtocolMode = "v16"
)

func (m ProtocolMode) String() string {
	if m == ProtocolV16 {
		return "0.16+ 新协议"
	}
	return "≤0.15 旧协议"
}
