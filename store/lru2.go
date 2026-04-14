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
