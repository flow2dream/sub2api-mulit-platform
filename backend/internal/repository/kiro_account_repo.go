package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.KiroOAuthRefreshSuccessRepository = (*accountRepository)(nil)
var _ service.KiroCooldownRepository = (*accountRepository)(nil)

func (r *accountRepository) SetKiroCooldownIfCredentialsUnchanged(ctx context.Context, a *service.Account, until time.Time, reason string) (bool, error) {
	credentials, err := json.Marshal(normalizeJSONMap(a.Credentials))
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `WITH updated AS (
UPDATE accounts SET temp_unschedulable_until = GREATEST(COALESCE(temp_unschedulable_until,$1),$1),
temp_unschedulable_reason = $2, updated_at = NOW()
WHERE id = $3 AND platform = $4 AND deleted_at IS NULL
AND credentials = $5::jsonb AND proxy_id IS NOT DISTINCT FROM $6
RETURNING id)
INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
SELECT $7,id,NULL,NULL FROM updated`, until, reason, a.ID, service.PlatformKiro, string(credentials), a.ProxyID, service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		r.syncSchedulerAccountSnapshotDetached(ctx, a.ID)
	}
	return n > 0, nil
}
