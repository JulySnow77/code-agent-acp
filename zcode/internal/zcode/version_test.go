package zcode

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in        string
		want      Version
		wantError bool
	}{
		{"0.16.5", Version{0, 16, 5}, false},
		{"zcode 0.16.5", Version{0, 16, 5}, false}, // 前缀噪声
		{"0.16.5-beta", Version{0, 16, 5}, false},  // 后缀
		{"1.2", Version{1, 2, 0}, false},           // 两段
		{"  0.14.8\n", Version{0, 14, 8}, false},   // 空白
		{"v2.1.0", Version{2, 1, 0}, false},        // v 前缀
		{"abc", Version{}, true},                   // 无数字
	}
	for _, c := range cases {
		got, err := ParseVersion(c.in)
		if c.wantError {
			if err == nil {
				t.Errorf("ParseVersion(%q) 应报错, got %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVersion(%q) 报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseVersion(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	a, _ := ParseVersion("0.16.5")
	b, _ := ParseVersion("0.16.6")
	c, _ := ParseVersion("0.16.5")
	d, _ := ParseVersion("0.15.9")
	if a.Compare(b) != -1 || b.Compare(a) != 1 || a.Compare(c) != 0 {
		t.Error("Compare 基本序错误")
	}
	if !a.AtLeast(d) || d.AtLeast(a) {
		t.Error("AtLeast 跨 minor 判定错误")
	}
	if !a.AtLeast(c) {
		t.Error("相等版本 AtLeast 应为真")
	}
	if (Version{}).Zero() != true || a.Zero() {
		t.Error("Zero 判定错误")
	}
	if a.String() != "0.16.5" {
		t.Errorf("String() = %s", a.String())
	}
}

func TestProtocolModeString(t *testing.T) {
	if ProtocolV16.String() == "" || ProtocolLegacy.String() == "" {
		t.Error("ProtocolMode.String 不应为空")
	}
}
