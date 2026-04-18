package store

import (
	"container/list"
	"sync"
	"time"
)

type lruCache struct {
	mu              sync.RWMutex             // 读写锁，保证线程安全
	list            *list.List               // 双向链表，保证LRU的顺序
	items           map[string]*list.Element // 键到链表节点的映射
	expires         map[string]time.Time     // 过期时间映射
	maxBytes        int64                    // 最大允许字节数
	usedBytes       int64                    // 当前使用的字节数
	onEvicted       func(key string, value Value)
	cleanupInterval time.Duration
	cleanupTicker   *time.Ticker
	closeCh         chan struct{} // 用于优雅关闭清理协程
}

// lruEntry 表示缓存中的一个条目
type lruEntry struct {
	key   string
	value Value
}

func newLRUCache(opts Options) *lruCache {
	c := &lruCache{
		list:            list.New(),
		items:           make(map[string]*list.Element),
		expires:         make(map[string]time.Time),
		maxBytes:        opts.MaxBytes,
		usedBytes:       0,
		onEvicted:       opts.OnEvicted,
		cleanupInterval: opts.CleanupInterval,
		closeCh:         make(chan struct{}),
	}
	if c.cleanupInterval <= 0 {
		c.cleanupInterval = time.Minute
	}
	c.cleanupTicker = time.NewTicker(c.cleanupInterval)
	go c.cleanupLoop()
	return c
}

// Len 获取缓存中的项数
func (c *lruCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.list.Len()
}

// Clear 清空缓存
func (c *lruCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 回调函数
	if c.onEvicted != nil {
		for _, e := range c.items {
			c.onEvicted(e.Value.(*lruEntry).key, e.Value.(*lruEntry).value)
		}
	}

	c.list.Init()
	c.expires = make(map[string]time.Time)
	c.items = make(map[string]*list.Element)
	c.usedBytes = 0
}

func (c *lruCache) cleanupLoop() {
	for {
		select {
		case <-c.cleanupTicker.C:
			c.mu.Lock()
			c.evict()
			c.mu.Unlock()
		case <-c.closeCh:
			return
		}
	}
}

func (c *lruCache) evict() {
	// 清理过期项
	for key, t := range c.expires {
		now := time.Now()
		if now.After(t) {
			if e, ok := c.items[key]; ok {
				c.removeElement(e)
			}
		}
	}
	// 清理最久未使用的项
	for c.maxBytes > 0 && c.list.Len() > 0 && c.usedBytes > c.maxBytes {
		e := c.list.Front()
		if e != nil {
			c.removeElement(e)
		}
	}
}

func (c *lruCache) removeElement(e *list.Element) {
	entry := e.Value.(*lruEntry)
	defer func() {
		if c.onEvicted != nil {
			c.onEvicted(entry.key, entry.value)
		}
	}()
	// 删除链表节点
	c.list.Remove(e)
	// 删除过期时间
	delete(c.expires, entry.key)
	// 删除缓存内容
	delete(c.items, entry.key)
	// 计算内存
	c.usedBytes -= int64(len(entry.key) + entry.value.Len())
}

// Delete 依据给出的key删除缓存中的项目
func (c *lruCache) Delete(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.removeElement(e)
		return true
	}
	return false
}

func (c *lruCache) Get(key string) (Value, bool) {
	c.mu.RLock()
	// 判断是否存在
	e, ok := c.items[key]
	if !ok {
		c.mu.RUnlock()
		return nil, false
	}
	// 判断是否过期
	t, hasEx := c.expires[key]
	if hasEx && time.Now().After(t) {
		c.mu.RUnlock()
		// 异步删除过期项目，避免在读锁内操作
		go c.Delete(key)
		return nil, false
	}
	// 获取值并释放锁
	val := e.Value.(*lruEntry).value
	c.mu.RUnlock()
	// 更新LRU位置
	c.mu.Lock()
	// 再次确认是否存在防止在写锁获取期间被其他协程删除
	if _, ok := c.items[key]; ok {
		c.list.MoveToBack(e)
	}
	c.mu.Unlock()

	return val, true
}

func (c *lruCache) SetWithExpiration(key string, value Value, ttl time.Duration) error {
	if value == nil {
		c.Delete(key)
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// 计算过期时间
	if ttl < 0 {
		delete(c.expires, key)
	} else {
		exp := time.Now().Add(ttl)
		// 更新过期时间
		c.expires[key] = exp
	}
	// 如果存在key则更新value
	if e, ok := c.items[key]; ok {
		entry := e.Value.(*lruEntry)
		c.usedBytes += int64(value.Len() - entry.value.Len())
		entry.value = value
		c.list.MoveToBack(e)
		return nil
	}
	// 不存在则添加
	newEntry := &lruEntry{key, value}
	c.items[key] = c.list.PushBack(newEntry)
	c.usedBytes += int64(len(key) + value.Len())

	// 添加新项后内存占用可能已超出限制，防止长时间超限，进行一次清理
	c.evict()

	return nil
}

func (c *lruCache) Set(key string, value Value) error {
	return c.SetWithExpiration(key, value, 0)
}

func (c *lruCache) GetWithExpiration(key string) (Value, time.Duration, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.items[key]
	if !ok {
		return nil, 0, false
	}
	// 是否存在有效期
	if t, hasEx := c.expires[key]; hasEx {
		// 过期
		if time.Now().After(t) {
			return nil, 0, false
		}
		// 计算剩余时间
		ttl := t.Sub(time.Now())
		c.list.MoveToBack(e)
		return e.Value.(*lruEntry).value, ttl, true
	}
	// 不存在有效期
	c.mu.Lock()
	c.list.MoveToBack(e)
	c.mu.Unlock()
	return e.Value.(*lruEntry).value, 0, true
}

// GetExpiration 获取键的过期时间
func (c *lruCache) GetExpiration(key string) (time.Time, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	expTime, ok := c.expires[key]
	return expTime, ok
}

// UpdateExpiration 更新过期时间
func (c *lruCache) UpdateExpiration(key string, expiration time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.items[key]; !ok {
		return false
	}

	if expiration > 0 {
		c.expires[key] = time.Now().Add(expiration)
	} else {
		delete(c.expires, key)
	}

	return true
}

// UsedBytes 返回当前使用的字节数
func (c *lruCache) UsedBytes() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.usedBytes
}

// MaxBytes 返回最大允许字节数
func (c *lruCache) MaxBytes() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.maxBytes
}

// SetMaxBytes 设置最大允许字节数并触发淘汰
func (c *lruCache) SetMaxBytes(maxBytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.maxBytes = maxBytes
	if maxBytes > 0 {
		c.evict()
	}
}

// Close 关闭缓存
func (c *lruCache) Close() {
	if c.cleanupTicker != nil {
		c.cleanupTicker.Stop()
		close(c.closeCh)
	}
}
