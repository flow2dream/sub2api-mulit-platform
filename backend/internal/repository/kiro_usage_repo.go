package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.KiroUsageRepository = (*usageLogRepository)(nil)
var _ service.KiroUsageLogReader = (*usageLogRepository)(nil)

func (r *usageLogRepository) EnrichKiroUsage(ctx context.Context, logs []service.UsageLog) error {
	// Bound SQL size for exports as well as paginated API reads.
	for start := 0; start < len(logs); start += 500 {
		end := min(start+500, len(logs))
		if err := r.enrichKiroUsageBatch(ctx, logs[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (r *usageLogRepository) enrichKiroUsageBatch(ctx context.Context, logs []service.UsageLog) error {
	type key struct {
		requestID string
		accountID int64
		apiKeyID  int64
	}
	indexes := make(map[key][]int, len(logs))
	args := make([]any, 0, len(logs)*3)
	values := make([]string, 0, len(logs))
	for i, log := range logs {
		if log.RequestID == "" {
			continue
		}
		k := key{log.RequestID, log.AccountID, log.APIKeyID}
		if _, exists := indexes[k]; !exists {
			values = append(values, fmt.Sprintf("($%d::text,$%d::bigint,$%d::bigint)", len(args)+1, len(args)+2, len(args)+3))
			args = append(args, log.RequestID, log.AccountID, log.APIKeyID)
		}
		indexes[k] = append(indexes[k], i)
	}
	if len(args) == 0 {
		return nil
	}
	rows, err := r.sql.QueryContext(ctx, `SELECT u.request_id,u.account_id,u.api_key_id,u.upstream_usage
FROM kiro_upstream_usage u JOIN (VALUES `+strings.Join(values, ",")+`) AS wanted(request_id,account_id,api_key_id)
ON u.request_id=wanted.request_id AND u.account_id=wanted.account_id AND u.api_key_id=wanted.api_key_id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k key
		var data []byte
		var usage kiro.Usage
		if err = rows.Scan(&k.requestID, &k.accountID, &k.apiKeyID, &data); err != nil {
			return err
		}
		if err = json.Unmarshal(data, &usage); err != nil {
			return err
		}
		for _, i := range indexes[k] {
			copyUsage := usage
			logs[i].UsageSource = usage.Source
			logs[i].UpstreamUsage = &copyUsage
		}
	}
	return rows.Err()
}

func (r *usageLogRepository) RecordKiroUsage(ctx context.Context, requestID string, accountID int64, apiKeyID int64, model string, usage kiro.Usage) error {
	data, err := json.Marshal(usage)
	if err != nil {
		return err
	}
	_, err = r.sql.ExecContext(ctx, `INSERT INTO kiro_upstream_usage (request_id,account_id,api_key_id,model,usage_source,upstream_usage)
VALUES ($1,$2,$3,$4,$5,$6::jsonb) ON CONFLICT (request_id,account_id,api_key_id) DO NOTHING`, requestID, accountID, apiKeyID, model, usage.Source, string(data))
	return err
}
