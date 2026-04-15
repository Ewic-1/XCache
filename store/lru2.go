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

func (c *lru2Cache) Get(key string) (Value, bool) {
	return nil, false
}

func (c *lru2Cache) Set(key string, value Value) error {
	return nil
}

func (c *lru2Cache) SetWithExpiration(key string, value Value, expiration time.Duration) error {
	return nil
}

func (c *lru2Cache) Delete(key string) bool {
	return false
}

func (c *lru2Cache) Clear() {

}

func (c *lru2Cache) Len() int {
	return 0
}

func (c *lru2Cache) Close() {

}

func (c *lru2Cache) cleanupLoop() {

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
		if c.m[idx-1].expireAt > 0 && !walker(c.m[idx-1].k, c.m[idx-1].v, c.m[idx-1].expireAt) {
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
	Z
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
	if idx, ok := c.hmap[key]; ok && c.m[idx-1].expireAt > 0 {
		e := c.m[idx-1].expireAt
		c.m[idx-1].expireAt = 0 // 标记为已删除
		c.move2tail(idx)        // 移动到链表尾部
		return &c.m[idx-1], 1, e
	}

	return nil, 0, 0
}
