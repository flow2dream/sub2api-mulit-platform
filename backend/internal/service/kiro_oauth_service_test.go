//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type kiroDeviceTestStore struct {
	mu         sync.Mutex
	data       map[string]KiroOAuthSession
	locks      map[string]string
	failDelete bool
}

func newKiroDeviceTestStore() *kiroDeviceTestStore {
	return &kiroDeviceTestStore{data: map[string]KiroOAuthSession{}, locks: map[string]string{}}
}
func (s *kiroDeviceTestStore) Save(_ context.Context, k string, v *KiroOAuthSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[k] = *v
	return nil
}
func (s *kiroDeviceTestStore) Load(_ context.Context, k string) (*KiroOAuthSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[k]
	if !ok {
		return nil, nil
	}
	return &v, nil
}
func (s *kiroDeviceTestStore) Delete(_ context.Context, k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failDelete {
		return errors.New("store failed")
	}
	delete(s.data, k)
	return nil
}
func (s *kiroDeviceTestStore) Lock(_ context.Context, k, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks[k] != "" {
		return false, nil
	}
	s.locks[k] = id
	return true, nil
}
func (s *kiroDeviceTestStore) Unlock(_ context.Context, k, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks[k] == id {
		delete(s.locks, k)
	}
	return nil
}

func kiroDeviceTestService(t *testing.T, status *string) (*KiroOAuthService, *kiroDeviceTestStore, *int) {
	t.Helper()
	store := newKiroDeviceTestStore()
	calls := 0
	u := &kiroTestUpstream{call: func(req *http.Request) (*http.Response, error) {
		calls++
		code := 200
		data := ""
		switch req.URL.Path {
		case "/client/register":
			data = fmt.Sprintf(`{"clientId":"c","clientSecret":"private-client","clientSecretExpiresAt":%d}`, time.Now().Add(time.Hour).Unix())
		case "/device_authorization":
			data = `{"deviceCode":"private-device","userCode":"USER-CODE","verificationUri":"https://view.awsapps.com/start/#/device","interval":5,"expiresIn":600}`
		case "/token":
			if *status != "completed" {
				code = 400
				data = fmt.Sprintf(`{"error":%q}`, *status)
			} else {
				data = `{"accessToken":"private-access","refreshToken":"private-refresh","expiresIn":3600}`
			}
		default:
			t.Fatalf("unexpected native endpoint: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(data))}, nil
	}}
	return NewKiroOAuthService(nil, u, store), store, &calls
}

func TestKiroDeviceOAuthIsolationThrottlingAndSingleDelivery(t *testing.T) {
	ctx := context.Background()
	status := "slow_down"
	s, store, calls := kiroDeviceTestService(t, &status)
	start, err := s.Start(ctx, 7, "us-east-1", "", nil)
	require.NoError(t, err)
	raw, err := json.Marshal(start)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "private-")
	_, err = s.Poll(ctx, 8, start.SessionID)
	require.Error(t, err)
	require.Equal(t, 2, *calls)
	pending, err := s.Poll(ctx, 7, start.SessionID)
	require.NoError(t, err)
	require.Equal(t, "pending", pending.Status)
	require.Equal(t, 2, *calls)
	key, _ := kiroSessionKey(7, start.SessionID)
	session, _ := store.Load(ctx, key)
	session.NextPollAt = 0
	require.NoError(t, store.Save(ctx, key, session))
	pending, err = s.Poll(ctx, 7, start.SessionID)
	require.NoError(t, err)
	require.Equal(t, 10, pending.RetryAfter)
	require.Equal(t, 3, *calls)
	_, err = s.Poll(ctx, 7, start.SessionID)
	require.NoError(t, err)
	require.Equal(t, 3, *calls)
	status = "completed"
	session, _ = store.Load(ctx, key)
	session.NextPollAt = 0
	require.NoError(t, store.Save(ctx, key, session))
	completed, err := s.Poll(ctx, 7, start.SessionID)
	require.NoError(t, err)
	require.Equal(t, "completed", completed.Status)
	require.Equal(t, "idc", completed.Credentials["auth_method"])
	require.Equal(t, "private-refresh", completed.Credentials["refresh_token"])
	_, err = s.Poll(ctx, 7, start.SessionID)
	require.Error(t, err)
	require.Equal(t, 4, *calls)
}

func TestKiroDeviceOAuthRejectsExpiredDeniedAndMissingOwner(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{"access_denied", "expired_token"} {
		t.Run(status, func(t *testing.T) {
			s, store, _ := kiroDeviceTestService(t, &status)
			start, err := s.Start(ctx, 7, "us-east-1", "", nil)
			require.NoError(t, err)
			key, _ := kiroSessionKey(7, start.SessionID)
			session, _ := store.Load(ctx, key)
			session.NextPollAt = 0
			require.NoError(t, store.Save(ctx, key, session))
			result, err := s.Poll(ctx, 7, start.SessionID)
			require.NoError(t, err)
			require.Contains(t, []string{"expired", "denied"}, result.Status)
			require.Nil(t, result.Credentials)
			left, _ := store.Load(ctx, key)
			require.Nil(t, left)
		})
	}
	status := "completed"
	s, store, calls := kiroDeviceTestService(t, &status)
	_, err := s.Start(ctx, 0, "us-east-1", "", nil)
	require.Error(t, err)
	require.Zero(t, *calls)
	start, err := s.Start(ctx, 7, "us-east-1", "", nil)
	require.NoError(t, err)
	key, _ := kiroSessionKey(7, start.SessionID)
	session, _ := store.Load(ctx, key)
	session.Login.ExpiresAt = time.Now().Unix() - 1
	require.NoError(t, store.Save(ctx, key, session))
	result, err := s.Poll(ctx, 7, start.SessionID)
	require.NoError(t, err)
	require.Equal(t, "expired", result.Status)
	require.Equal(t, 2, *calls)
}

func TestKiroDeviceOAuthDoesNotDeliverCredentialsIfConsumptionFails(t *testing.T) {
	ctx := context.Background()
	status := "completed"
	s, store, _ := kiroDeviceTestService(t, &status)
	start, err := s.Start(ctx, 7, "us-east-1", "", nil)
	require.NoError(t, err)
	key, _ := kiroSessionKey(7, start.SessionID)
	session, _ := store.Load(ctx, key)
	session.NextPollAt = 0
	require.NoError(t, store.Save(ctx, key, session))
	store.failDelete = true
	result, err := s.Poll(ctx, 7, start.SessionID)
	require.Error(t, err)
	require.Nil(t, result)
	require.NotContains(t, err.Error(), "private-")
}

func TestKiroDeviceOAuthConcurrentPollDeliversCredentialsOnce(t *testing.T) {
	ctx := context.Background()
	status := "completed"
	s, store, calls := kiroDeviceTestService(t, &status)
	start, err := s.Start(ctx, 7, "us-east-1", "", nil)
	require.NoError(t, err)
	key, _ := kiroSessionKey(7, start.SessionID)
	session, _ := store.Load(ctx, key)
	session.NextPollAt = 0
	require.NoError(t, store.Save(ctx, key, session))
	var wg sync.WaitGroup
	var completed atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.Poll(ctx, 7, start.SessionID)
			if err == nil && result.Credentials != nil {
				completed.Add(1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, completed.Load())
	require.Equal(t, 3, *calls)
}
