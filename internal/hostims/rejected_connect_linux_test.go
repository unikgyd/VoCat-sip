//go:build linux

package hostims

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestMMConnectRejectsOnlyAcknowledgedMobileEquipmentFailures(t *testing.T) {
	for _, failure := range []error{
		dbus.Error{Name: mmService + ".Error.MobileEquipment.Ipv6OnlyAllowed", Body: []any{"private subscriber diagnostic"}},
		&dbus.Error{Name: mmService + ".Error.MobileEquipment.MissingOrUnknownApn"},
	} {
		got := modemManagerConnectError(failure)
		if !errors.Is(got, ErrRejected) || strings.Contains(got.Error(), "private") {
			t.Fatal("negative modem reply was not safely classified", got)
		}
	}
	for _, failure := range []error{
		context.DeadlineExceeded, context.Canceled,
		dbus.Error{Name: "org.freedesktop.DBus.Error.NoReply"},
		dbus.Error{Name: mmService + ".Error.Core.Cancelled"},
		dbus.Error{Name: mmService + ".Error.Core.Timeout"},
	} {
		if !errors.Is(modemManagerConnectError(failure), ErrTransport) {
			t.Fatal("unknown result was treated as an acknowledged rejection")
		}
	}
}
