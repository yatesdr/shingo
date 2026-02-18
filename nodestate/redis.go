package nodestate

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

type RedisStore struct {
	client *redis.Client
}

func NewRedisStore(client *redis.Client) *RedisStore {
	return &RedisStore{client: client}
}

func inventoryKey(nodeID int64) string {
	return fmt.Sprintf("warpath:node:%d:inventory", nodeID)
}

func metaKey(nodeID int64) string {
	return fmt.Sprintf("warpath:node:%d:meta", nodeID)
}

func countKey(nodeID int64) string {
	return fmt.Sprintf("warpath:node:%d:count", nodeID)
}

const allNodesKey = "warpath:nodes"

func (r *RedisStore) SetNodePayloads(ctx context.Context, nodeID int64, items []PayloadItem) error {
	data, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, inventoryKey(nodeID), data, 0).Err()
}

func (r *RedisStore) GetNodePayloads(ctx context.Context, nodeID int64) ([]PayloadItem, error) {
	data, err := r.client.Get(ctx, inventoryKey(nodeID)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var items []PayloadItem
	return items, json.Unmarshal(data, &items)
}

func (r *RedisStore) UpdateNodeMeta(ctx context.Context, nodeID int64, meta *NodeMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	pipe := r.client.Pipeline()
	pipe.Set(ctx, metaKey(nodeID), data, 0)
	pipe.SAdd(ctx, allNodesKey, nodeID)
	_, err = pipe.Exec(ctx)
	return err
}

func (r *RedisStore) GetNodeMeta(ctx context.Context, nodeID int64) (*NodeMeta, error) {
	data, err := r.client.Get(ctx, metaKey(nodeID)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var meta NodeMeta
	return &meta, json.Unmarshal(data, &meta)
}

func (r *RedisStore) SetCount(ctx context.Context, nodeID int64, count int) error {
	return r.client.Set(ctx, countKey(nodeID), count, 0).Err()
}

func (r *RedisStore) GetCount(ctx context.Context, nodeID int64) (int, error) {
	val, err := r.client.Get(ctx, countKey(nodeID)).Int()
	if err == redis.Nil {
		return 0, nil
	}
	return val, err
}

func (r *RedisStore) IncrementCount(ctx context.Context, nodeID int64) error {
	return r.client.Incr(ctx, countKey(nodeID)).Err()
}

func (r *RedisStore) DecrementCount(ctx context.Context, nodeID int64) error {
	return r.client.Decr(ctx, countKey(nodeID)).Err()
}

func (r *RedisStore) GetAllNodeIDs(ctx context.Context) ([]int64, error) {
	members, err := r.client.SMembers(ctx, allNodesKey).Result()
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(members))
	for _, m := range members {
		id, err := strconv.ParseInt(m, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (r *RedisStore) RemoveNode(ctx context.Context, nodeID int64) error {
	pipe := r.client.Pipeline()
	pipe.Del(ctx, inventoryKey(nodeID), metaKey(nodeID), countKey(nodeID))
	pipe.SRem(ctx, allNodesKey, nodeID)
	_, err := pipe.Exec(ctx)
	return err
}

func (r *RedisStore) FlushAll(ctx context.Context) error {
	ids, err := r.GetAllNodeIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		r.RemoveNode(ctx, id)
	}
	return r.client.Del(ctx, allNodesKey).Err()
}
