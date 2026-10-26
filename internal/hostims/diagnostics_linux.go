//go:build linux

package hostims

import (
	"context"
	"errors"
)

// PreparationFailureCode exposes only trusted constant labels. Modem, DBus,
// APDU and kernel error text may contain identities or keys and is not logged.
func PreparationFailureCode(err error) string {
	if err == nil {
		return ""
	}
	for _, item := range []struct {
		err  error
		code string
	}{
		{context.DeadlineExceeded, "deadline"}, {context.Canceled, "canceled"},
		{ErrSIMApplication, "sim_application"}, {ErrSIMIdentity, "sim_identity"},
		{ErrSIMAuthentication, "sim_authentication"}, {ErrSIM, "sim_binding"},
		{ErrQMIProxy, "qmi_proxy"}, {ErrQMIEndpoint, "qmi_endpoint"},
		{ErrServingCellUnavailable, "serving_cell"}, {ErrPCSCF, "pcscf"},
		{ErrRuntime, "wds_runtime"}, {ErrNetworkNotReady, "network_not_ready"},
		{ErrNetworkConflict, "network_conflict"}, {ErrNetwork, "kernel_network"},
		{ErrChanged, "identity_changed"}, {ErrParameters, "bearer_parameters"},
		{ErrRejected, "bearer_rejected"}, {ErrTransport, "modemmanager"},
		{ErrUncertain, "outcome_uncertain"}, {ErrSIMReplay, "aka_replay_fence"},
		{ErrSecurityScope, "kernel_security"},
		{ErrXFRMConflict, "security_conflict"},
	} {
		if errors.Is(err, item.err) {
			return item.code
		}
	}
	return "unclassified"
}
