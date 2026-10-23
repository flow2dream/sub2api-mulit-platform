package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

type kiroOAuthSessionStore struct{ rdb *redis.Client }

func NewKiroOAuthSessionStore(rdb *redis.Client) service.KiroOAuthSessionStore {
	return &kiroOAuthSessionStore{rdb: rdb}
}

const kiroOAuthPrefix = "oauth:kiro:device:"

func (s *kiroOAuthSessionStore) Save(ctx context.Context, id string, session *service.KiroOAuthSession) error {
	data, err := json.Marshal(session)
	if err != nil {
		return err
	}
	ttl := time.Until(time.Unix(session.Login.ExpiresAt, 0))
	if ttl <= 0 {
		return errors.New("kiro OAuth session expired")
	}
	return s.rdb.Set(ctx, kiroOAuthPrefix+id, data, ttl).Err()
}
func (s *kiroOAuthSessionStore) Load(ctx context.Context, id string) (*service.KiroOAuthSession, error) {
	data, err := s.rdb.Get(ctx, kiroOAuthPrefix+id).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var session service.KiroOAuthSession
	if err = json.Unmarshal(data, &session); err != nil {
		return nil, err
	}
	return &session, nil
}
func (s *kiroOAuthSessionStore) Delete(ctx context.Context, id string) error {
	return s.rdb.Del(ctx, kiroOAuthPrefix+id).Err()
}
func (s *kiroOAuthSessionStore) Lock(ctx context.Context, id, token string) (bool, error) {
	return s.rdb.SetNX(ctx, kiroOAuthPrefix+"lock:"+id, token, 30*time.Second).Result()
}
func (s *kiroOAuthSessionStore) Unlock(ctx context.Context, id, token string) error {
	return s.rdb.Eval(ctx, `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) else return 0 end`, []string{kiroOAuthPrefix + "lock:" + id}, token).Err()
}
