package singleflight

import "sync"

type call struct {
	val interface{}
	wg  sync.WaitGroup
	err error
}

type Group struct {
	m sync.Map
}

func (g *Group) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
	// 如果已有call，则等待结果并返回
	if v, ok := g.m.Load(key); ok {
		existing := v.(*call)
		existing.wg.Wait()
		return existing.val, existing.err
	}
	// 如果没有，则成为第一个call
	c := &call{}
	c.wg.Add(1)
	g.m.Store(key, c)
	defer g.m.Delete(key)
	defer c.wg.Done()
	c.val, c.err = fn()
	// 返回
	return c.val, c.err
}
