//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestKiroPersistenceIntegration(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	accounts := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	a := mustCreateAccount(t, tx.Client(), &service.Account{Name: "kiro-integration", Platform: service.PlatformKiro, Type: service.AccountTypeOAuth, Credentials: map[string]any{"refresh_token": "original", "access_token": "old"}})
	expected := a.Credentials
	rotated := map[string]any{"refresh_token": "rotated", "access_token": "new"}
	updated, err := accounts.UpdateKiroOAuthCredentialsIfUnchanged(ctx, a.ID, expected, a.ProxyID, rotated)
	require.NoError(t, err)
	require.True(t, updated)
	updated, err = accounts.UpdateKiroOAuthCredentialsIfUnchanged(ctx, a.ID, expected, a.ProxyID, map[string]any{"refresh_token": "stale"})
	require.NoError(t, err)
	require.False(t, updated)
	until := time.Now().Add(time.Minute)
	updated, err = accounts.SetKiroCooldownIfCredentialsUnchanged(ctx, a, until, "stale throttle")
	require.NoError(t, err)
	require.False(t, updated)
	fresh, err := accounts.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "rotated", fresh.GetCredential("refresh_token"))
	updated, err = accounts.SetKiroCooldownIfCredentialsUnchanged(ctx, fresh, until, "throttled")
	require.NoError(t, err)
	require.True(t, updated)
	after, err := accounts.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.WithinDuration(t, until, *after.TempUnschedulableUntil, time.Second)
	usageRepo := newUsageLogRepositoryWithSQL(tx.Client(), tx)
	credits := 0.5
	usage := kiro.Usage{InputTokens: 12, OutputTokens: 4, Source: "estimated", Credits: &credits, Complete: true}
	for i := 0; i < 2; i++ {
		require.NoError(t, usageRepo.RecordKiroUsage(ctx, "billing-id", a.ID, 11, "native", usage))
	}
	logs := []service.UsageLog{{RequestID: "billing-id", AccountID: a.ID, APIKeyID: 11}, {RequestID: "billing-id", AccountID: a.ID, APIKeyID: 12}}
	require.NoError(t, usageRepo.EnrichKiroUsage(ctx, logs))
	require.Equal(t, "estimated", logs[0].UsageSource)
	require.Equal(t, 0.5, *logs[0].UpstreamUsage.Credits)
	require.Nil(t, logs[1].UpstreamUsage)
}
