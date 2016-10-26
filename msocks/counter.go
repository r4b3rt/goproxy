package msocks

import (
	"github.com/shell909090/goproxy/sutils"
	"sync/atomic"
)

type SpeedCounter struct {
	cnt uint32
	Spd uint32
	All uint64
}

func NewSpeedCounter() (sc *SpeedCounter) {
	sc = &SpeedCounter{}
	sutils.UpdateAdd(sc)
	return sc
}

func (sc *SpeedCounter) Close() error {
	return sutils.UpdateRemove(sc)
}

func (sc *SpeedCounter) Update() {
	c := atomic.SwapUint32(&sc.cnt, 0)
	sc.Spd = c / sutils.UPDATE_INTERVAL
	sc.All += uint64(c)
}

func (sc *SpeedCounter) Add(s uint32) uint32 {
	return atomic.AddUint32(&sc.cnt, s)
}
