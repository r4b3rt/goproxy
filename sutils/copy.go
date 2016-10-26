package sutils

import (
	"io"
	"sync"
)

var (
	BufferPool = sync.Pool{
		New: func () interface{} {
			return make([]byte, 8192)
		},
	}
)

func CopyLink(dst, src io.ReadWriteCloser) {
	go func() {
		defer src.Close()
		buf := BufferPool.Get().([]byte)
		defer BufferPool.Put(buf)
		io.CopyBuffer(src, dst, buf)
	}()
	defer dst.Close()
	buf := BufferPool.Get().([]byte)
	defer BufferPool.Put(buf)
	io.CopyBuffer(dst, src, buf)
}
