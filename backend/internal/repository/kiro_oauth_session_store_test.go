//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestKiroDeviceOAuthRedisTTLAndLockOwnership(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	s := NewKiroOAuthSessionStore(rdb)
	other := NewKiroOAuthSessionStore(rdb)
	ctx := context.Background()
	session := &service.KiroOAuthSession{Login: kiro.DeviceLogin{ExpiresAt: time.Now().Add(time.Minute).Unix(), ClientSecret: "test-secret"}, AuthRegion: "us-east-1"}
	require.NoError(t, s.Save(ctx, "7:id", session))
	loaded, err := other.Load(ctx, "7:id")
	require.NoError(t, err)
	require.Equal(t, session, loaded)
	locked, err := s.Lock(ctx, "7:id", "first")
	require.NoError(t, err)
	require.True(t, locked)
	require.NoError(t, other.Unlock(ctx, "7:id", "wrong-owner"))
	locked, err = other.Lock(ctx, "7:id", "second")
	require.NoError(t, err)
	require.False(t, locked)
	require.NoError(t, s.Unlock(ctx, "7:id", "first"))
	locked, err = other.Lock(ctx, "7:id", "second")
	require.NoError(t, err)
	require.True(t, locked)
	server.FastForward(2 * time.Minute)
	loaded, err = s.Load(ctx, "7:id")
	require.NoError(t, err)
	require.Nil(t, loaded)
	locked, err = s.Lock(ctx, "7:id", "third")
	require.NoError(t, err)
	require.True(t, locked)
}
