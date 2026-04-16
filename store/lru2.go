package store

import (
	"sync"
	"sync/atomic"
	"time"
)

var (
	clock = time.Now().UnixNano()
	prev  = uint16(0)
	next  = uint16(1)
)

func Now() int64 {
	return atomic.LoadInt64(&clock)
}

func init() {
	go func() {
		for {
			atomic.StoreInt64(&clock, time.Now().UnixNano()) // 每秒校准一次
			for i := 0; i < 9; i++ {
				time.Sleep(100 * time.Millisecond)
				atomic.AddInt64(&clock, int64(100*time.Millisecond)) // 保持 clock 在一个精确的时间范围内，同时避免频繁的系统调用
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
}

type lru2Cache struct {
	locks         []sync.Mutex
	caches        [][2]*cache
	mask          int32
	cleanupTicker *time.Ticker
	onEvicted     func(key string, value Value)
	closeCh       chan struct{}
}

type cache struct {
	// dlnk为数组模拟的双向链表，d[i][j]中j为0/1表示d[i]节点的前驱/后继
	// dlnk[0]为哨兵节点，前驱为链表尾节点，后继为链表头节点
	dlnk [][2]uint16
	m    []node            // 预分配内存存储节点
	hmap map[string]uint16 //key到节点的索引
	last uint16            // 最后一个节点的索引
}

type node struct {
	k        string
	v        Value
	expireAt int64 // 纳秒时间戳，性能比time.Time好
}

func Create(cap uint16) *cache {
	return &cache{
		dlnk: make([][2]uint16, cap+1),
		m:    make([]node, cap),
		hmap: make(map[string]uint16, cap),
		last: 0,
	}
}

func newLRU2Cache(opts Options) *lru2Cache {
	if opts.CapPerBucket <= 0 {
		opts.CapPerBucket = 1024
	}
	if opts.BucketCount <= 0 {
		opts.BucketCount = 16
	}
	if opts.Level2Cap <= 0 {
		opts.Level2Cap = 1024
	}
	if opts.CleanupInterval <= 0 {
		opts.CleanupInterval = time.Minute
	}

	mask := maskOfNextPowOf2(opts.BucketCount)
	c := &lru2Cache{
		locks:         make([]sync.Mutex, mask+1),
		caches:        make([][2]*cache, mask+1),
		onEvicted:     opts.OnEvicted,
		cleanupTicker: time.NewTicker(opts.CleanupInterval),
		mask:          int32(mask),
		closeCh:       make(chan struct{}),
	}

	for i := range c.caches {
		c.caches[i][0] = Create(opts.CapPerBucket)
		c.caches[i][1] = Create(opts.Level2Cap)
	}

	go c.cleanupLoop()

	return c
}

// maskOfNextPowOf2 计算大于或等于输入值的最近 2 的幂次方减一作为掩码值
func maskOfNextPowOf2(cap uint16) uint16 {
	if cap > 0 && cap&(cap-1) == 0 {
		return cap - 1
	}

	// 通过多次右移和按位或操作，将二进制中最高的 1 位右边的所有位都填充为 1
	cap |= cap >> 1
	cap |= cap >> 2
	cap |= cap >> 4

	return cap | (cap >> 8)
}

// 实现了 BKDR 哈希算法，用于计算键的哈希值
func hashBKRD(s string) (hash int32) {
	for i := 0; i < len(s); i++ {
		hash = hash*131 + int32(s[i])
	}

	return
}

// 将一个节点移动到链表头
func (c *cache) move2head(idx uint16) {
	// 如果在链表头部则跳过
	if c.dlnk[idx][prev] != 0 {
		// 如果在链表中要先删除
		c.dlnk[c.dlnk[idx][prev]][next] = c.dlnk[idx][next]
		c.dlnk[c.dlnk[idx][next]][prev] = c.dlnk[idx][prev]
		// 插入头部
		c.dlnk[idx][next] = c.dlnk[0][next]
		c.dlnk[c.dlnk[idx][next]][prev] = idx
		c.dlnk[0][next] = idx
		c.dlnk[idx][prev] = 0
	}
}

func (c *cache) move2tail(idx uint16) {
	if c.dlnk[idx][next] != 0 {
		c.dlnk[c.dlnk[idx][prev]][next] = c.dlnk[idx][next]
		c.dlnk[c.dlnk[idx][next]][prev] = c.dlnk[idx][prev]

		c.dlnk[idx][prev] = c.dlnk[0][prev]
		c.dlnk[c.dlnk[idx][prev]][next] = idx
		c.dlnk[idx][next] = 0
		c.dlnk[0][prev] = idx
	}
}

// 遍历缓存中的所有有效项
func (c *cache) walk(walker func(key string, value Value, expireAt int64) bool) {
	for idx := c.dlnk[0][next]; idx != 0; idx = c.dlnk[idx][next] {
		if !walker(c.m[idx-1].k, c.m[idx-1].v, c.m[idx-1].expireAt) {
			return
		}
	}
}

// 向缓存中添加项，如果是新增返回 1，更新返回 0
func (c *cache) put(k string, v Value, expireAt int64, onEvicted func(string, Value)) int {
	// 查到已有映射则走更新逻辑
	if idx, ok := c.hmap[k]; ok {
		c.m[idx-1].v, c.m[idx-1].expireAt = v, expireAt
		c.move2head(idx)
		return 0
	}

	// 如果容量已满
	if c.last == uint16(cap(c.m)) {
		tailIdx := c.dlnk[0][prev]
		tail := &c.m[tailIdx-1]
		if onEvicted != nil && tail.expireAt > 0 {
			onEvicted(tail.k, tail.v)
		}
		delete(c.hmap, tail.k)
		tail.k = k
		tail.v = v
		tail.expireAt = expireAt
		c.hmap[k] = tailIdx
		c.move2head(tailIdx)
		return 1
	}

	// 还有空位，新增节点
	c.last++
	idx := c.last
	c.m[idx-1].k = k
	c.m[idx-1].v = v
	c.m[idx-1].expireAt = expireAt
	c.hmap[k] = idx
	// 插到链表头
	if len(c.hmap) == 1 {
		// 第一个节点
		c.dlnk[0][prev] = idx
		c.dlnk[0][next] = idx
		c.dlnk[idx][prev] = 0
		c.dlnk[idx][next] = 0
	} else {
		oldHead := c.dlnk[0][next]
		c.dlnk[idx][prev] = 0
		c.dlnk[idx][next] = oldHead
		c.dlnk[oldHead][prev] = idx
		c.dlnk[0][next] = idx
	}
	return 1
}

// 从缓存中获取键对应的节点和状态
func (c *cache) get(key string) (*node, int) {
	if idx, ok := c.hmap[key]; ok {
		c.move2head(idx)
		return &c.m[idx-1], 1
	}
	return nil, 0
}

// 从缓存中删除键对应的项
func (c *cache) del(key string) (*node, int, int64) {
	if idx, ok := c.hmap[key]; ok {
		e := c.m[idx-1].expireAt
		c.m[idx-1].expireAt = -1 // 标记为已删除
		c.move2tail(idx)         // 移动到链表尾部
		return &c.m[idx-1], 1, e
	}

	return nil, 0, 0
}
func (c *lru2Cache) SetWithExpiration(k string, v Value, expiration time.Duration) error {
	expireAt := int64(0)
	if expiration > 0 {
		expireAt = Now() + int64(expiration.Nanoseconds())
	}
	idx := hashBKRD(k) & c.mask
	c.locks[idx].Lock()
	defer c.locks[idx].Unlock()
	// 放入一级缓存
	c.caches[idx][0].put(k, v, expireAt, c.onEvicted)
	return nil
}

func (c *lru2Cache) Set(k string, v Value) error {
	return c.SetWithExpiration(k, v, 0)
}

func (c *lru2Cache) Get(k string) (Value, bool) {
	// 计算idx，加锁等
	idx := hashBKRD(k) & c.mask
	c.locks[idx].Lock()
	defer c.locks[idx].Unlock()
	now := Now()
	// 查一级缓存
	if n, ok, expireAt := c.caches[idx][0].del(k); ok == 1 {
		// 查到缓存
		if expireAt > 0 && expireAt <= now {
			// 过期则删除
			c.delete(k, idx)
			return nil, false
		}

		// 有效则移动至二级缓存
		c.caches[idx][1].put(k, n.v, expireAt, c.onEvicted)
		return n.v, true
	}

	// 没查到则查二级缓存
	if n, ok := c._get(k, idx, 1); ok == 1 {
		// 过期则删除
		if n.expireAt > 0 && n.expireAt <= now {
			c.caches[idx][1].del(k)
			c.delete(k, idx)
			return nil, false
		}
		return n.v, true
	}
	return nil, false
}

func (c *lru2Cache) _get(k string, idx int32, level int32) (*node, int) {
	if n, st := c.caches[idx][level].get(k); st > 0 && n != nil {
		currentTime := Now()
		if n.expireAt == -1 {
			return nil, 0
		}
		if n.expireAt > 0 && currentTime >= n.expireAt {
			// 过期或已删除
			return nil, 0
		}
		return n, st
	}

	return nil, 0
}

func (c *lru2Cache) delete(key string, idx int32) bool {
	n1, s1, _ := c.caches[idx][0].del(key)
	n2, s2, _ := c.caches[idx][1].del(key)
	deleted := s1 > 0 || s2 > 0

	if deleted && c.onEvicted != nil {
		if n1 != nil && n1.v != nil {
			c.onEvicted(key, n1.v)
		} else if n2 != nil && n2.v != nil {
			c.onEvicted(key, n2.v)
		}
	}

	if deleted {
		//c.expirations.Delete(key)
	}

	return deleted
}

func (c *lru2Cache) Delete(key string) bool {
	idx := hashBKRD(key) & c.mask
	c.locks[idx].Lock()
	defer c.locks[idx].Unlock()

	return c.delete(key, idx)
}

func (c *lru2Cache) Clear() {
	var keys []string

	for i := range c.caches {
		c.locks[i].Lock()

		c.caches[i][0].walk(func(key string, value Value, expireAt int64) bool {
			keys = append(keys, key)
			return true
		})
		c.caches[i][1].walk(func(key string, value Value, expireAt int64) bool {
			// 检查键是否已经收集（避免重复）
			for _, k := range keys {
				if key == k {
					return true
				}
			}
			keys = append(keys, key)
			return true
		})

		c.locks[i].Unlock()
	}

	for _, key := range keys {
		c.Delete(key)
	}

	//c.expirations = sync.Map{}
}

func (c *lru2Cache) Len() (count int) {
	for i := range c.caches {
		c.locks[i].Lock()

		c.caches[i][0].walk(func(key string, value Value, expireAt int64) bool {
			count++
			return true
		})
		c.caches[i][1].walk(func(key string, value Value, expireAt int64) bool {
			count++
			return true
		})

		c.locks[i].Unlock()
	}

	return
}

// Close 关闭缓存，停止清理协程
func (c *lru2Cache) Close() {
	if c.cleanupTicker != nil {
		c.cleanupTicker.Stop()
		close(c.closeCh)
	}
}

// cleanupLoop 定期清理过期缓存的协程
func (c *lru2Cache) cleanupLoop() {
	for {
		select {
		case <-c.cleanupTicker.C:
			currentTime := Now()

			for i := range c.caches {
				c.locks[i].Lock()

				// 检查并清理过期项目
				var expiredKeys []string

				c.caches[i][0].walk(func(key string, value Value, expireAt int64) bool {
					if expireAt > 0 && currentTime >= expireAt {
						expiredKeys = append(expiredKeys, key)
					}
					return true
				})

				c.caches[i][1].walk(func(key string, value Value, expireAt int64) bool {
					if expireAt > 0 && currentTime >= expireAt {
						for _, k := range expiredKeys {
							if key == k {
								// 避免重复
								return true
							}
						}
						expiredKeys = append(expiredKeys, key)
					}
					return true
				})

				for _, key := range expiredKeys {
					c.delete(key, int32(i))
				}

				c.locks[i].Unlock()
			}
		case <-c.closeCh:
			return
		}
	}
}
