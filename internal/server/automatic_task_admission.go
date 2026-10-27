package server

import (
	"context"
	"errors"
	"time"

	"vocat/internal/store"
)

// Only the failed initial lease acquisition can produce this marker. A busy
// error after execution starts must never be mistaken for permission to replay.
type automaticTaskAdmissionError struct{ err error }

func (e automaticTaskAdmissionError) Error() string { return e.err.Error() }
func (e automaticTaskAdmissionError) Unwrap() error { return e.err }

func (s *Server) executeAutomaticTaskWhenAvailable(ctx context.Context, task store.AutomaticTask, progress automaticTaskProgress) (string, error) {
	waiting := false
	for {
		output, err := s.executeAutomaticTask(ctx, task, progress)
		var admission automaticTaskAdmissionError
		if !errors.As(err, &admission) || !errors.Is(admission.err, errLineBusy) {
			return output, err
		}
		if !waiting && progress != nil {
			progress("正在等待线路操作完成，尚未提交业务")
		}
		waiting = true
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}
