package service

import (
	"context"
	"fmt"
)

// KiroUsageLogReader enriches only already-authorized log rows. Implementations
// must match account, API key and billing request ID, never request ID alone.
type KiroUsageLogReader interface {
	EnrichKiroUsage(context.Context, []UsageLog) error
}

func (s *UsageService) enrichKiroUsage(ctx context.Context, logs []UsageLog) error {
	if reader, ok := s.usageRepo.(KiroUsageLogReader); ok && len(logs) > 0 {
		if err := reader.EnrichKiroUsage(ctx, logs); err != nil {
			return fmt.Errorf("read upstream metering: %w", err)
		}
	}
	return nil
}
