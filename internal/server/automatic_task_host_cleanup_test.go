package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"vocat/internal/hostimsruntime"
	"vocat/internal/store"
)

type taskCleanupController struct {
	*dispatchTestController
	pending int
	failure error
	ids     []string
}

func (c *taskCleanupController) StopDevice(_ context.Context, id string) error {
	c.ids = append(c.ids, id)
	if c.pending > 0 {
		c.pending--
		return hostimsruntime.ErrCleanup
	}
	return c.failure
}

func TestAutomaticTaskHostCleanupWaitKeepsOriginalOwnerAndFence(t *testing.T) {
	for _, scenario := range []string{"settles", "deadline", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			s := blockedRegionServer(t, "310260123456789")
			controller := &taskCleanupController{dispatchTestController: &dispatchTestController{}, pending: 1}
			s.hostIMS = controller
			ctx := context.Background()
			if scenario == "deadline" {
				controller.pending = 100
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			} else if scenario == "unknown" {
				controller.pending, controller.failure = 0, errors.New("unverified retained owner")
			}
			err := s.stopAutomaticTaskController(ctx, "original-line", store.IMSModeHost)
			_, fenced := s.telephonyCleanup.Load(telephonyCleanupKey{"original-line", store.IMSModeHost})
			if scenario == "settles" {
				if err != nil || len(controller.ids) != 2 || fenced {
					t.Fatal("confirmed same-owner cleanup did not complete", err, controller.ids, fenced)
				}
			} else if scenario == "deadline" {
				if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, hostimsruntime.ErrCleanup) || !fenced {
					t.Fatal("deadline released an unconfirmed owner", err, fenced)
				}
			} else if err == nil || len(controller.ids) != 1 || !fenced {
				t.Fatal("unknown cleanup was retried or accepted", err, controller.ids, fenced)
			}
			for _, id := range controller.ids {
				if id != "original-line" {
					t.Fatal("cleanup moved to another line")
				}
			}
		})
	}
}
