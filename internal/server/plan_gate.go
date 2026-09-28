package server

import (
	"net/http"

	"inno-live-server/internal/plan"
)

// planGateError는 플랜이 허용하지 않는 송출 방식을 막는다(#273). 이유별로 코드를
// 나눠 클라이언트가 업그레이드 안내에 쓰게 한다. 플랜이 없는 세션(인증을 끈
// 벤치)은 막지 않는다.
func planGateError(owner plan.Plan, fhd bool, targets int) *apiError {
	if owner == "" {
		return nil
	}
	mode := plan.ModeFor(fhd, targets)
	if owner.Allows(mode) {
		return nil
	}
	details := map[string]any{"plan": owner, "mode": mode}
	policy, _ := owner.Policy()
	switch {
	case len(policy.AllowedModes) == 0:
		return &apiError{Status: http.StatusForbidden, Code: "plan_server_streaming_not_allowed", Message: "This plan streams from the device, not through the server.", Details: details}
	case fhd && !owner.Allows(plan.ModeFHDSingle):
		return &apiError{Status: http.StatusForbidden, Code: "plan_resolution_not_allowed", Message: "This plan does not include FHD streaming.", Details: details}
	default:
		return &apiError{Status: http.StatusForbidden, Code: "plan_simulcast_not_allowed", Message: "This plan does not include simultaneous streaming at this resolution.", Details: details}
	}
}
