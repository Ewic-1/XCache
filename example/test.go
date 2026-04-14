package main

import (
	"xcache"
	"context"
	"fmt"
	"time"
)

func must(cond bool, format string, args ...any) {
	if !cond {
		panic(fmt.Sprintf(format, args...))
	}
}

func main() {
	ctx := context.Background()

	fmt.Println("[1/6] 创建 Group")
	getter := xcache.GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
		// 后续实现 loadData 时可以用到这个回源函数
		return []byte("from-getter:" + key), nil
	})

	g := xcache.NewGroup(
		"scores",
		2<<20,
		getter,
		xcache.WithExpiration(2*time.Second),
	)
	defer g.Close()

	fmt.Println("[2/6] 测试 Set + 本地命中 Get")
	err := g.Set(ctx, "Tom", []byte("630"))
	must(err == nil, "Set failed: %v", err)

	v, err := g.Get(ctx, "Tom")
	must(err == nil, "Get failed: %v", err)
	must(v.String() == "630", "unexpected value, got=%q want=%q", v.String(), "630")
	fmt.Println("Tom =>", v.String())

	fmt.Println("[3/6] 测试 Delete")
	err = g.Delete(ctx, "Tom")
	must(err == nil, "Delete failed: %v", err)

	// 这里不直接断言 miss 结果，因为当前的 loadData 还未完成
	_, _ = g.Get(ctx, "Tom")
	fmt.Println("Delete 后已尝试再次读取（loadData 待实现）")

	fmt.Println("[4/6] 测试参数校验")
	err = g.Set(ctx, "", []byte("1"))
	must(err == xcache.ErrKeyRequired, "want ErrKeyRequired, got=%v", err)

	err = g.Set(ctx, "Jerry", nil)
	must(err == xcache.ErrValueRequired, "want ErrValueRequired, got=%v", err)

	_, err = g.Get(ctx, "")
	must(err == xcache.ErrKeyRequired, "want ErrKeyRequired, got=%v", err)

	fmt.Println("[5/6] 测试 ListGroups")
	names := xcache.ListGroups()
	found := false
	for _, n := range names {
		if n == "scores" {
			found = true
			break
		}
	}
	must(found, "group 'scores' not found in ListGroups: %v", names)

	fmt.Println("[6/6] 测试 Close")
	err = g.Close()
	must(err == nil, "Close failed: %v", err)

	err = g.Set(ctx, "AfterClose", []byte("1"))
	must(err == xcache.ErrGroupClosed, "want ErrGroupClosed after Close, got=%v", err)

	fmt.Println("当前阶段测试通过")
}
