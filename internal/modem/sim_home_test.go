package modem

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestReaderHomeOptionalClassificationCannotHideCleanupOrCancellation(t *testing.T) {
	for _, v := range []struct {
		err  error
		want bool
	}{
		{nil, false}, {ErrHomeMNCLengthUnavailable, true}, {fmt.Errorf("read: %w", ErrHomeMNCLengthUnavailable), true},
		{errors.Join(ErrHomeMNCLengthUnavailable, ErrHomeMNCLengthUnavailable), true},
		{errors.Join(ErrHomeMNCLengthUnavailable, context.Canceled), false},
		{errors.Join(ErrHomeMNCLengthUnavailable, errors.New("cleanup")), false},
		{errors.New(ErrHomeMNCLengthUnavailable.Error()), false},
	} {
		if got := OnlyHomeMNCLengthUnavailable(v.err); got != v.want {
			t.Fatalf("classification %v = %v want %v", v.err, got, v.want)
		}
	}
}
