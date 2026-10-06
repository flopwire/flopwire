package main

import (
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/busproto"
)

func TestDeliveryStatusRedeliveryMayHaveBeenShown(t *testing.T) {
	for _, attempt := range []int{1, 2} {
		out := deliveryStatus([]busproto.DeliveryFailure{{ID: "mstatus", Reason: "unconfirmed", Attempt: attempt}})
		if strings.Contains(out, "may have been shown before") != (attempt > 1) || strings.Contains(out, "earlier print") {
			t.Fatalf("attempt %d: %s", attempt, out)
		}
		if !strings.Contains(out, "Message mstatus delivery status: unconfirmed.") {
			t.Fatal(out)
		}
	}
}
