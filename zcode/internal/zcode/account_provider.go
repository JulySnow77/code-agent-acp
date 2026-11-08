// Package zcode 的账号 provider 快照: 让无头 (headless) 启动也能看到订阅型模型。
//
// 背景 (移植自上游 TS 版 src/config/account-provider.ts):
// 3.12+ app-server 的 provider registry 由三个来源合成 —— 内置表
// zcode-builtin.json、个人的 provider_config.json、以及桌面宿主根据登录用户
// 订阅状态计算并通过 provider/updateAccountConfig 推送的 ACCOUNT 快照。
// 无头启动没有宿主可推送, 而「从凭证推导 entitlement」并不可靠 (实测:
// 同一台机器上每个 account:* provider 都读成 entitled:false, 而桌面运行
// 时 account:bigmodel-individual-coding-plan 是有权的 —— 用户配置里选中的
// GLM 模型整个从 settings.model.available 消失, session/setModel 报
// "Provider Registry 中不存在 Model")。
//
// 自己推这份快照即可恢复一致 (实测: 推送后 settings.model.available 从 3
// 个第三方模型变成 21 个, 含 account:bigmodel-individual-coding-plan/GLM-5.3,
// 切换成功)。
package zcode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// builtinProviderConfigEnv 是 CLI 用来定位内置 provider 表的环境变量。
const builtinProviderConfigEnv = "ZCODE_BUILTIN_PROVIDER_CONFIG_FILE"

// builtinProviderFileName 是内置 provider 表的文件名。
const builtinProviderFileName = "zcode-builtin.json"

// AccountProviderConfigMethod 是快照推送用的 JSON-RPC 方法名。
const AccountProviderConfigMethod = "provider/updateAccountConfig"

// builtinRule 是内置表里的一条 provider 规则 (只取用得到的字段)。
type builtinRule struct {
	ProviderID   string `json:"providerId"`
	ProviderName string `json:"providerName"`
	Config       struct {
		Group           string   `json:"group"`
		BuiltinModelIDs []string `json:"builtinModelIds"`
		Visibility      string   `json:"visibility"`
		Access          struct {
			Type        string `json:"type"`
			Mode        string `json:"mode"`
			AccountType string `json:"accountType"`
			Entitled    bool   `json:"entitled"`
		} `json:"access"`
	} `json:"config"`
}

// builtinTable 是内置 provider 表的顶层结构。
type builtinTable struct {
	Revision int64 `json:"revision"`
	Config   struct {
		ProviderConfigRules struct {
			ProviderRules []builtinRule `json:"providerRules"`
		} `json:"providerConfigRules"`
	} `json:"config"`
}

// accountState 是快照里一个 provider 的账号状态。
type accountState struct {
	Availability string `json:"availability"`
	Entitled     bool   `json:"entitled"`
	Current      bool   `json:"current"`
}

// accountAccess 是快照里一个 provider 的访问方式。
type accountAccess struct {
	Type     string `json:"type"`
	Entitled bool   `json:"entitled"`
}

// accountProviderEntry 是快照里一个 provider 的模型清单。
type accountProviderEntry struct {
	BuiltinModelIDs []string      `json:"builtinModelIds,omitempty"`
	Access          accountAccess `json:"access"`
}

// AccountProviderConfig 是 provider/updateAccountConfig 的载荷。
type AccountProviderConfig struct {
	Revision                   string                          `json:"revision"`
	BasedOnZCodeBuiltinRevisio string                          `json:"basedOnZCodeBuiltinRevision"`
	Providers                  map[string]accountProviderEntry `json:"providers"`
	States                     map[string]accountState         `json:"states"`
}

// releaseArchDir 复现 ZCode 的 runtime/provider 目录命名。
// 上游 services/src/runtime-tools/clientPlatform.ts 的两张映射表是
// Node 架构名 → 发布架构名:
//
//	x64   → x86_64
//	arm64 → aarch64
//
// 直接用 runtime.GOARCH (amd64) 会去找 darwin-amd64/ 这个不存在的目录,
// 于是永远回落 bundled 副本, 指纹也就永远对不上。
// platform 侧 Go 的 GOOS 已经是 windows 而非 Node 的 win32, 无需映射。
func releaseArchDir() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}

// resolveRuntimeProviderVersion 决定用哪个版本目录。
//
// 顺序: 显式 ZCODE_APP_VERSION → 从 ZCODE_BIN 所在 .app 的 Info.plist 读
// CFBundleShortVersionString → 都拿不到时由 semverMaxDirName 兜底。
// 不做这一步会跨版本误选: 本机同时留着 3.12.3 ~ 3.14.4 与 0.0.0-dev,
// 随便挑一个算出来的指纹后端都不认。
func resolveRuntimeProviderVersion() string {
	if v := strings.TrimSpace(os.Getenv("ZCODE_APP_VERSION")); v != "" {
		return v
	}
	if bin := strings.TrimSpace(os.Getenv("ZCODE_BIN")); bin != "" {
		// <App>.app/Contents/Resources/... → <App>.app/Contents/Info.plist
		contents := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(bin))))
		if v := readBundleShortVersion(filepath.Join(contents, "Info.plist")); v != "" {
			return v
		}
	}
	return ""
}

// readBundleShortVersion 从 Info.plist 里读 CFBundleShortVersionString。
// 只做正则取值而非解析 plist: 这个文件只有版本这一项需要, 引入 plist
// 解析器不值得, 而 Info.plist 是 XML 单键文本, 正则足够稳。
func readBundleShortVersion(plistPath string) string {
	data, err := os.ReadFile(plistPath)
	if err != nil {
		return ""
	}
	re := regexp.MustCompile(`<key>CFBundleShortVersionString</key>\s*<string>([^<]+)</string>`)
	m := re.FindSubmatch(data)
	if len(m) != 2 {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}

// semverMaxDirName 在候选目录名里挑"最高"的一个, 跳过 0.0.0-dev 这类
// 开发占位。比较只按数字段逐段比, 段数不同按缺 0 处理。
func semverMaxDirName(names []string) string {
	best, bestKey := "", -1
	for _, n := range names {
		fields := strings.Split(n, ".")
		nums := make([]int, 0, len(fields))
		ok := len(fields) > 0
		for _, f := range fields {
			v, err := strconv.Atoi(f)
			if err != nil {
				ok = false
				break
			}
			nums = append(nums, v)
		}
		if !ok {
			continue
		}
		key := 0
		for _, v := range nums {
			key = key*1000 + v
		}
		if key > bestKey {
			best, bestKey = n, key
		}
	}
	return best
}

// runtimeScopedBuiltinProviderPath 定位桌面按 endpoint 隔离下载的内置表。
//
// 3.14+ 的桌面端不再直接读 App 包里那份 bundled 副本, 而是把内置表
// 物化到
//
//	~/.zcode/v2/runtime/provider/<platform>-<arch>/<version>/endpoint-<hash>/zcode-builtin.json
//
// 再交给 app-server。basedOnZCodeBuiltinRevision 是
// "zcode-builtin:<revision>:<sha256(路径)>" —— 路径取错指纹就取错, 后端
// 会**接受推送但静默丢弃 entitlement** (实测: 用户明明有订阅, 目录里仍然
// 没有任何 account:* 模型)。
//
// 取「当前 App 版本 + 最新修改时间」的那份, 与桌面行为一致; 找不到返回
// 空串, 让调用方回落到 env / bundled。
func runtimeScopedBuiltinProviderPath() string {
	root := filepath.Join(zcodeV2Dir(""), "runtime", "provider")
	if root == "" {
		return ""
	}
	archDir := filepath.Join(root, runtime.GOOS+"-"+releaseArchDir())
	versionDirs, err := os.ReadDir(archDir)
	if err != nil {
		return ""
	}
	appVersion := resolveRuntimeProviderVersion()
	// 版本未知时不逐个目录猜, 直接取 semver 最大的那个; 目录全是非数字
	// 形式 (或为空) 才逐目录扫描, 保证还能捡到 dev 快照。
	targets := []string{appVersion}
	if appVersion == "" {
		var names []string
		for _, vdir := range versionDirs {
			if vdir.IsDir() {
				names = append(names, vdir.Name())
			}
		}
		targets = []string{semverMaxDirName(names)}
		if targets[0] == "" {
			targets = names
		}
	}
	bestPath, bestMod := "", time.Time{}
	for _, vdir := range versionDirs {
		if !vdir.IsDir() {
			continue
		}
		// 版本目录形如 3.14.4。appVersion 已知时只认它, 免得拿旧版本
		// 残留副本算出一个后端不认的指纹。
		if !slices.Contains(targets, vdir.Name()) {
			continue
		}
		endpoints, err := os.ReadDir(filepath.Join(archDir, vdir.Name()))
		if err != nil {
			continue
		}
		for _, ep := range endpoints {
			if !ep.IsDir() {
				continue
			}
			path := filepath.Join(archDir, vdir.Name(), ep.Name(), builtinProviderFileName)
			st, err := os.Stat(path)
			if err != nil {
				continue
			}
			if bestPath == "" || st.ModTime().After(bestMod) {
				bestPath, bestMod = path, st.ModTime()
			}
		}
	}
	return bestPath
}

// BuiltinProviderPath 定位内置 provider 表。
//
// 顺序很重要: 先看注入 env (DeriveEnv 算出的值, 也就是 CLI 实际会读的那份),
// 再看环境变量本身。直接读环境变量在桌面宿主上是错的 —— 它指向一个按版本
// 编号的 runtime/provider 拷贝, 对它取哈希得到的 revision 会被后端拒绝
// (实测: 推送报告「已接收」但 entitlement 从未生效)。
func BuiltinProviderPath() string {
	if p := runtimeScopedBuiltinProviderPath(); p != "" {
		return p
	}
	if v := os.Getenv(builtinProviderConfigEnv); strings.TrimSpace(v) != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		var candidates []string
		switch runtime.GOOS {
		case "windows":
			if la := os.Getenv("LOCALAPPDATA"); la != "" {
				candidates = append(candidates, filepath.Join(la, "Programs", "ZCode", "resources", "config", "provider", builtinProviderFileName))
			}
		case "darwin":
			candidates = append(candidates, "/Applications/ZCode.app/Contents/Resources/config/provider/"+builtinProviderFileName)
		default:
			candidates = append(candidates,
				filepath.Join("/opt/ZCode/resources/config/provider", builtinProviderFileName),
				filepath.Join("/usr/share/ZCode/resources/config/provider", builtinProviderFileName),
			)
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
		_ = home
	}
	return ""
}

// readBuiltinTable 读取并解析内置 provider 表。
func readBuiltinTable(warn func(string)) (*builtinTable, string, bool) {
	file := BuiltinProviderPath()
	if file == "" {
		return nil, "", false
	}
	data, err := os.ReadFile(file)
	if err != nil {
		if warn != nil {
			warn("account-provider: 内置 provider 表读取失败 (" + file + "): " + err.Error())
		}
		return nil, "", false
	}
	var t builtinTable
	if err := json.Unmarshal(data, &t); err != nil {
		if warn != nil {
			warn("account-provider: 内置 provider 表结构异常 (" + file + "): " + err.Error())
		}
		return nil, "", false
	}
	return &t, file, true
}

// accountRules 返回内置表里所有 zhipu-account 类型的 provider 规则。
func accountRules(t *builtinTable) []builtinRule {
	var out []builtinRule
	for _, r := range t.Config.ProviderConfigRules.ProviderRules {
		if r.Config.Access.Type == "zhipu-account" {
			out = append(out, r)
		}
	}
	return out
}

// builtinRevision 复现运行时为内置表推导的 revision:
// zcode-builtin:<revision>:<sha256(路径)>。
//
// 哈希覆盖的是解析后的「路径」而非文件内容 (已对照运行时自己打印的
// configRevision 验证), 所以必须用我们交给后端的同一个路径来算。
func builtinRevision(file string, revision int64) string {
	abs, err := filepath.Abs(file)
	if err != nil {
		abs = file
	}
	sum := sha256.Sum256([]byte(abs))
	return "zcode-builtin:" + strconv.FormatInt(revision, 10) + ":" + hex.EncodeToString(sum[:])
}

// ConfigProviderIDFor 把 account: provider id 反查成 config.json 里的
// builtin: id (授权信号都是用后者说的)。
//
// 内置表里 account:bigmodel-individual-coding-plan 对应 config.json 的
// builtin:bigmodel-coding-plan; 其余 plan 拼写一一对应。表不存在时
// (纯无头 Linux, 没有桌面 App 可读) 走兜底正则 —— 这些对应关系是上游冻结的
// (session-store 迁移 0020 + legacy-model-provider-identity.ts)。
func ConfigProviderIDFor(providerID string) string {
	if !strings.HasPrefix(providerID, "account:") {
		return providerID
	}
	if t, _, ok := readBuiltinTable(nil); ok {
		for _, r := range accountRules(t) {
			if r.ProviderID != providerID {
				continue
			}
			family, mode := r.Config.Access.AccountType, r.Config.Access.Mode
			if family != "" && mode != "" {
				plan := mode
				if mode == "individual-coding-plan" {
					plan = "coding-plan"
				}
				return "builtin:" + family + "-" + plan
			}
		}
	}
	// team 与家族共用 legacy key (它自己没有 builtin 拼写)。
	if m := regexp.MustCompile(`^account:([a-z0-9]+)-(start-plan|(?:individual-|team-)?coding-plan)$`).
		FindStringSubmatch(providerID); m != nil {
		plan := m[2]
		if plan != "start-plan" {
			plan = "coding-plan"
		}
		return "builtin:" + m[1] + "-" + plan
	}
	return providerID
}

// AccountProviderIDFor 是 ConfigProviderIDFor 的正向映射 (config.json 的
// builtin: id → 注册表实际暴露的 account: id)。
func AccountProviderIDFor(providerID string) string {
	if !strings.HasPrefix(providerID, "builtin:") {
		return providerID
	}
	t, _, ok := readBuiltinTable(nil)
	if !ok {
		return providerID
	}
	slug := strings.TrimPrefix(providerID, "builtin:")
	// "<family>-<plan>" —— 按第一个短横切分, 多段家族名才能存活。
	dash := strings.Index(slug, "-")
	if dash <= 0 {
		return providerID
	}
	family, plan := slug[:dash], slug[dash+1:]
	if plan == "coding-plan" {
		plan = "individual-coding-plan"
	}
	for _, r := range accountRules(t) {
		if r.Config.Access.AccountType == family && r.Config.Access.Mode == plan {
			return r.ProviderID
		}
	}
	return providerID
}

// zcodeV2Dir 返回 ~/.zcode/v2 目录 (凭据与授权缓存都在这)。
//
// credsPath 非空时直接取其所在目录 —— 调用方 (测试、doctor) 由此把整套判定
// 指向夹具, 而不必改动全局 HOME。ZCODE_HOME 是 zcode 自己认的环境变量, 在此
// 同样生效。
func zcodeV2Dir(credsPath string) string {
	if credsPath != "" {
		return filepath.Dir(credsPath)
	}
	if home := strings.TrimSpace(os.Getenv("ZCODE_HOME")); home != "" {
		return filepath.Join(home, "v2")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".zcode", "v2")
}

// defaultCredsPath 解析真实的凭证文件路径 (与 zcodeV2Dir 同一套优先级)。
func defaultCredsPath() string {
	dir := zcodeV2Dir("")
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "config.json")
}

// entitledBuiltinProviders 推导用户实际持有的订阅, 以 config.json 里的
// legacy builtin:<family>-<plan> id 为键。三个磁盘信号取并集, 任何单个过期
// 仍能得到真相, 按权威性排序:
//
//  1. coding-plan-cache.json —— 桌面自己的裁决结果
//     ({items: {"builtin:bigmodel-coding-plan": {status: "available"}}}),
//     带的是已解析的可用/未授权结论, 不是原始标志。
//  2. config.json (legacy CLI 配置) —— builtin:* 条目中 enabled:true 且有 apiKey。
//     App 至今仍在写, 作为兜底。
//  3. setting.json —— 设置界面里用户选过什么。这只是「选择」记录而不是授权
//     (实测: 它会把一个未授权的 zai plan 也列进来), 因此绝不与上面两个
//     权威来源取并集, 只在前两者都为空时作为最后手段。
//
// 2026-09 对照桌面自己推送的快照验证: 该推导得到的授权集合与桌面完全一致。
func entitledBuiltinProviders(credsPath string) map[string]bool {
	out := map[string]bool{}
	dir := zcodeV2Dir(credsPath)
	if dir == "" {
		return out
	}

	// 1) 桌面授权缓存。
	if data, err := os.ReadFile(filepath.Join(dir, "coding-plan-cache.json")); err == nil {
		var cache struct {
			EntryStatus struct {
				Items map[string]struct {
					Status string `json:"status"`
				} `json:"items"`
			} `json:"entryStatus"`
		}
		if json.Unmarshal(data, &cache) == nil {
			for pid, entry := range cache.EntryStatus.Items {
				if entry.Status == "available" {
					out[pid] = true
				}
			}
		}
	}

	// 2) legacy config.json 的 provider 启用状态。
	if credsPath == "" {
		credsPath = defaultCredsPath()
	}
	if data, err := os.ReadFile(credsPath); err == nil {
		var cfg struct {
			Provider map[string]struct {
				Enabled bool `json:"enabled"`
				Options struct {
					APIKey string `json:"apiKey"`
				} `json:"options"`
			} `json:"provider"`
		}
		if json.Unmarshal(data, &cfg) == nil {
			for pid, p := range cfg.Provider {
				if !strings.HasPrefix(pid, "builtin:") {
					continue
				}
				if p.Enabled && strings.TrimSpace(p.Options.APIKey) != "" {
					out[pid] = true
				}
			}
		}
	}
	if len(out) > 0 {
		return out
	}

	// 3) 仅当前两者都为空时 (机器从未写过它们): 设置界面记下的选择。
	if data, err := os.ReadFile(filepath.Join(dir, "setting.json")); err == nil {
		var setting struct {
			ModelProviderFamilySelectedKeys map[string]string `json:"modelProviderFamilySelectedKeys"`
		}
		if json.Unmarshal(data, &setting) == nil {
			re := regexp.MustCompile(`(builtin:[a-z0-9-]+)`)
			keys := make([]string, 0, len(setting.ModelProviderFamilySelectedKeys))
			for k := range setting.ModelProviderFamilySelectedKeys {
				keys = append(keys, k)
			}
			sort.Strings(keys) // map 遍历无序, 排序保证可复现
			for _, k := range keys {
				if m := re.FindStringSubmatch(setting.ModelProviderFamilySelectedKeys[k]); m != nil {
					out[m[1]] = true
				}
			}
		}
	}
	return out
}

// CodingPlanAPIKeyFor 返回桥能为某个 account:* coding-plan 模型提供的凭证,
// 无法提供时返回空串。
//
// 3.12+ 后端在 account provider 上每次发模型请求前都会向宿主索要 provider
// runtime headers (打包验证: 除桌面宿主外唯一的供给方是 CLI 自己的
// "standalone" 凭证对, 其 identity 段这台机器从未写过 —— api-key 段在 CLI 的
// 加密凭证库里, 这里读不到)。桌面用订阅的 API key 作答; 桥可以用同一个订阅
// 在 legacy config.json 里的 key —— 也就是 3.12 之前那个 builtin: provider 用的
// 同一把。
//
// 无 start-plan: start-plan 没有独立 API key, 它用登录 JWT
// (zcodejwttoken) 直接打 zcode-plan 端点 —— 见下面的 startPlan 分支。
// team/off-peak 的 key 是存储加密的, 仍然读不到。
func CodingPlanAPIKeyFor(accountProviderID, credsPath string) string {
	if !strings.HasPrefix(accountProviderID, "account:") {
		return ""
	}
	t, _, ok := readBuiltinTable(nil)
	if !ok {
		return ""
	}
	found := false
	startPlan := false
	for _, r := range accountRules(t) {
		if r.ProviderID == accountProviderID {
			switch r.Config.Access.Mode {
			case "start-plan":
				startPlan = true
			case "individual-coding-plan":
			default:
				return ""
			}
			found = true
			break
		}
	}
	if startPlan {
		// 订阅成立才提供 —— 否则该 provider 本来就不该出现在目录里,
		// 给了 key 也没人会请求。
		if !FetchStartPlanModels(credsPath, nil).Entitled {
			return ""
		}
		jwt, err := LoadZcodeJWTToken(credsPath)
		if err != nil {
			return ""
		}
		return jwt
	}
	if !found {
		return ""
	}
	legacyID := ConfigProviderIDFor(accountProviderID)
	if legacyID == accountProviderID {
		return ""
	}
	if !entitledBuiltinProviders(credsPath)[legacyID] {
		return ""
	}
	if credsPath == "" {
		credsPath = defaultCredsPath()
		if credsPath == "" {
			return ""
		}
	}
	data, err := os.ReadFile(credsPath)
	if err != nil {
		return ""
	}
	var cfg struct {
		Provider map[string]struct {
			Options struct {
				APIKey string `json:"apiKey"`
			} `json:"options"`
		} `json:"provider"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	return strings.TrimSpace(cfg.Provider[legacyID].Options.APIKey)
}

// BuildAccountProviderConfig 组装账号快照: 内置表声明的每个 coding-plan
// provider, 授权状态取自 config.json 里已启用的 builtin plan。
// 没有可推送内容 (无表 / 无 zhipu provider) 时返回 nil。
func BuildAccountProviderConfig(credsPath string, warn func(string)) *AccountProviderConfig {
	t, file, ok := readBuiltinTable(warn)
	if !ok {
		return nil
	}
	rules := accountRules(t)
	if len(rules) == 0 {
		return nil
	}
	enabled := entitledBuiltinProviders(credsPath)
	payload := &AccountProviderConfig{
		Revision:                   "account:bridge:" + strconv.FormatInt(time.Now().UnixMilli(), 10),
		BasedOnZCodeBuiltinRevisio: builtinRevision(file, t.Revision),
		Providers:                  map[string]accountProviderEntry{},
		States:                     map[string]accountState{},
	}
	// Start Plan 的模型清单只在服务端 (见 FetchStartPlanModels)。这里
	// 至多查一次 —— 只有当内置表确实声明了 start-plan provider 时才值得
	// 解凭证、打网络。
	var startPlanState *StartPlanState
	startPlanLoaded := false
	loadStartPlan := func() StartPlanState {
		if !startPlanLoaded {
			s := FetchStartPlanModels(credsPath, warn)
			startPlanState, startPlanLoaded = &s, true
		}
		return *startPlanState
	}
	for _, r := range rules {
		legacyID := ConfigProviderIDFor(r.ProviderID)
		entitled := enabled[legacyID]
		models := append([]string(nil), r.Config.BuiltinModelIDs...)
		if r.Config.Access.Mode == "start-plan" {
			// Start Plan 权益是**按家族**判定的: 我们查的是 bigmodel 那条
			// billing 链路 (zcode.z.ai + zcodejwttoken), 把它套到
			// account:zai-start-plan 上会让一个查都没查过的套餐凭空有权,
			// 然后在选择器里给出一个必然跑不通的选项。非 bigmodel 家族
			// 维持 entitledBuiltinProviders 的磁盘推导结果。
			if r.Config.Access.AccountType != "bigmodel" {
				if warn != nil && enabled[legacyID] {
					warn(fmt.Sprintf("%s 属于未覆盖的套餐家族, 沿用磁盘推导的授权状态", r.ProviderID))
				}
			} else if s := loadStartPlan(); s.Entitled {
				entitled = true
				if len(s.Models) > 0 {
					models = append([]string(nil), s.Models...)
				}
			} else if warn != nil {
				warn(fmt.Sprintf("start-plan 权益未确认 (%s), %s 沿用内置表清单",
					s.Detail, r.ProviderID))
			}
		}
		payload.Providers[r.ProviderID] = accountProviderEntry{
			BuiltinModelIDs: models,
			Access:          accountAccess{Type: "zhipu-account", Entitled: entitled},
		}
		availability := "unavailable"
		if entitled {
			availability = "available"
		}
		payload.States[r.ProviderID] = accountState{
			Availability: availability,
			Entitled:     entitled,
			Current:      entitled,
		}
	}
	return payload
}
