//go:build zcode_diagnostic

package zcode

import (
	"fmt"
	"testing"
)

// T024 临时诊断（用后即删）：打印 Start Plan 服务端判定与账户快照组装结果。
func TestT024DiagnosticAccountSnapshot(t *testing.T) {
	state := FetchStartPlanModels("", func(w string) { t.Logf("warn: %s", w) })
	t.Logf("start-plan: entitled=%v models=%v detail=%s", state.Entitled, state.Models, state.Detail)
	payload := BuildAccountProviderConfig("", func(w string) { t.Logf("warn: %s", w) })
	if payload == nil {
		t.Fatalf("BuildAccountProviderConfig 返回 nil（无内置表或无 zhipu provider）")
	}
	t.Logf("revision: %s", payload.Revision)
	t.Logf("basedOn: %s", payload.BasedOnZCodeBuiltinRevisio)
	for id, p := range payload.Providers {
		t.Logf("provider %s: models=%v access=%+v", id, p.BuiltinModelIDs, p.Access)
	}
	for id, s := range payload.States {
		t.Logf("state %s: %+v", id, s)
	}
	if st, ok := payload.States["account:bigmodel-start-plan"]; ok {
		fmt.Printf("STARTPLAN-STATE: %+v\n", st)
	} else {
		fmt.Println("STARTPLAN-STATE: absent")
	}
}
