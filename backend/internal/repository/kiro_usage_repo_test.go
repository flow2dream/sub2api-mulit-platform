package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestKiroUsageEnrichmentMatchesAccountAndRequest(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &usageLogRepository{sql: db}
	logs := []service.UsageLog{{RequestID: "same-request", AccountID: 7, APIKeyID: 11}, {RequestID: "same-request", AccountID: 7, APIKeyID: 12}, {RequestID: "other-request", AccountID: 7, APIKeyID: 11}}
	mock.ExpectQuery(`(?s)SELECT u.request_id,u.account_id,u.api_key_id,u.upstream_usage.*ON u.request_id=wanted.request_id AND u.account_id=wanted.account_id AND u.api_key_id=wanted.api_key_id`).
		WithArgs("same-request", int64(7), int64(11), "same-request", int64(7), int64(12), "other-request", int64(7), int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"request_id", "account_id", "api_key_id", "upstream_usage"}).AddRow("same-request", 7, 11, `{"source":"mixed","input_tokens":10,"credits":0.5,"complete":true}`))
	require.NoError(t, r.EnrichKiroUsage(context.Background(), logs))
	require.Equal(t, "mixed", logs[0].UsageSource)
	require.Equal(t, 0.5, *logs[0].UpstreamUsage.Credits)
	require.Nil(t, logs[1].UpstreamUsage)
	require.Nil(t, logs[2].UpstreamUsage)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKiroMeteringInsertIsIdempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &usageLogRepository{sql: db}
	credits := 0.25
	usage := kiro.Usage{Source: "estimated", Credits: &credits}
	for i := 0; i < 2; i++ {
		mock.ExpectExec(`(?s)INSERT INTO kiro_upstream_usage.*ON CONFLICT \(request_id,account_id,api_key_id\) DO NOTHING`).
			WithArgs("billing-request", int64(7), int64(11), "native-model", "estimated", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, int64(1-i)))
		require.NoError(t, r.RecordKiroUsage(context.Background(), "billing-request", 7, 11, "native-model", usage))
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKiroCooldownCASDoesNotOverwriteChangedCredentials(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := newAccountRepositoryWithSQL(nil, db, nil)
	a := &service.Account{ID: 7, Platform: service.PlatformKiro, Credentials: map[string]any{"refresh_token": "original"}}
	mock.ExpectExec(`(?s)WITH updated AS.*AND credentials = \$5::jsonb AND proxy_id IS NOT DISTINCT FROM \$6.*FROM updated`).
		WithArgs(sqlmock.AnyArg(), "throttled", int64(7), service.PlatformKiro, `{"refresh_token":"original"}`, nil, service.SchedulerOutboxEventAccountChanged).
		WillReturnResult(sqlmock.NewResult(0, 0))
	changed, err := r.SetKiroCooldownIfCredentialsUnchanged(context.Background(), a, a.CreatedAt, "throttled")
	require.NoError(t, err)
	require.False(t, changed)
	require.NoError(t, mock.ExpectationsWereMet())
}
