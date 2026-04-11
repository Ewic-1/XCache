package XCache

// ByteView 只读的字节视图，用来缓存数据
type ByteView struct {
	b []byte
}

func (bv ByteView) Len() int {
	return len(bv.b)
}

func (bv ByteView) Bytes() []byte {
	return cloneBytes(bv.b)
}

func (bv ByteView) String() string {
	return string(bv.b)
}

func cloneBytes(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
