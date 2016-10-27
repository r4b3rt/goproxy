package msocks

import (
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ST_UNKNOWN = iota
	ST_SYN_RECV
	ST_SYN_SENT
	ST_EST
	ST_CLOSE_WAIT
	ST_FIN_WAIT
)

type Conn struct {
	lock     sync.Mutex
	status   uint8
	sess     *Session
	streamid uint16
	sender   FrameSender
	ch_syn   chan uint32
	Network  string
	Address  string

	rlock    sync.Mutex // this should used to block reader and reader, not writer
	rbufsize int32
	r_rest   []byte
	rqueue   *Queue

	wlock    sync.RWMutex
	wbufsize int32
	wev      *sync.Cond
}

func NewConn(streamid uint16, sess *Session, network, address string) (c *Conn) {
	c = &Conn{
		status:   ST_UNKNOWN,
		sess:     sess,
		streamid: streamid,
		sender:   sess,
		Network:  network,
		Address:  address,
		rqueue:   NewQueue(),
	}
	c.wev = sync.NewCond(&c.wlock)
	return
}

func RecvWithTimeout(ch chan uint32, t time.Duration) (errno uint32) {
	var ok bool
	ch_timeout := time.After(t)
	select {
	case errno, ok = <-ch:
		if !ok {
			return ERR_CLOSED
		}
	case <-ch_timeout:
		return ERR_TIMEOUT
	}
	return
}

func (c *Conn) GetStreamId() uint16 {
	return c.streamid
}

func (c *Conn) GetAddress() (s string) {
	return fmt.Sprintf("%s:%s", c.Network, c.Address)
}

func (c *Conn) String() (s string) {
	return fmt.Sprintf("%d(%d)", c.sess.LocalPort(), c.streamid)
}

func (c *Conn) SendSynAndWait() (err error) {
	c.ch_syn = make(chan uint32, 0)

	err = c.CheckAndSetStatus(ST_UNKNOWN, ST_SYN_SENT)
	if err != nil {
		return
	}

	fb := NewFrameSyn(c.streamid, c.Network, c.Address)
	err = c.sess.SendFrame(fb)
	if err != nil {
		log.Error("%s", err)
		go c.Final()
		return
	}

	errno := RecvWithTimeout(c.ch_syn, DIAL_TIMEOUT*time.Second)

	if errno != ERR_NONE {
		log.Error("remote connect %s failed for %d.", c.String(), errno)
		go c.Final()
	} else {
		err = c.CheckAndSetStatus(ST_SYN_SENT, ST_EST)
		if err != nil {
			return
		}
		log.Notice("%s connected: %s => %s.", c.Network, c.String(), c.Address)
	}

	c.ch_syn = nil
	return
}

func (c *Conn) Final() {
	err := c.sess.RemovePort(c.streamid)
	if err != nil {
		log.Error("%s", err)
		return
	}

	log.Notice("%s final.", c.String())

	c.lock.Lock()
	defer c.lock.Unlock()
	if c.status != ST_UNKNOWN {
		c.rqueue.Close()
	}
	c.status = ST_UNKNOWN
	return
}

func (c *Conn) Close() (err error) {
	log.Info("call Close %s.", c.String())

	c.lock.Lock()
	switch c.status {
	case ST_UNKNOWN, ST_FIN_WAIT:
		// maybe call close twice
		c.lock.Unlock()
		log.Error("unexpected status %d, maybe try to close a closed conn.")
		return
	case ST_EST:
		c.status = ST_FIN_WAIT
		c.lock.Unlock()
		log.Info("%s closed from local.", c.String())

		fb := NewFrameFin(c.streamid)
		err = c.sender.SendFrame(fb)
		if err != nil {
			log.Info("%s", err)
			return
		}
	case ST_CLOSE_WAIT:
		c.lock.Unlock()

		fb := NewFrameFin(c.streamid)
		err = c.sender.SendFrame(fb)
		if err != nil {
			log.Info("%s", err)
			return
		}
		go c.Final()
	default:
		c.lock.Unlock()
		err = ErrUnknownState
		log.Error("%s", err.Error())
		return
	}
	return
}

func (c *Conn) SendFrame(f Frame) (err error) {
	switch ft := f.(type) {
	default:
		err = ErrUnexpectedPkg
		log.Error("%s", err)
		err = c.Close()
		if err != nil {
			log.Error("%s", err.Error())
		}
		return
	case *FrameResult:
		return c.InConnect(ft.Errno)
	case *FrameData:
		return c.InData(ft)
	case *FrameWnd:
		return c.InWnd(ft)
	case *FrameFin:
		return c.InFin(ft)
	case *FrameRst:
		log.Debug("reset %s.", c.String())
		go c.Final()
	}
	return
}

func (c *Conn) InConnect(errno uint32) (err error) {
	c.lock.Lock()
	if c.status != ST_SYN_SENT {
		c.lock.Unlock()
		err = ErrNotSyn
		log.Error("%s", err.Error())
		return
	}
	c.lock.Unlock()

	select {
	case c.ch_syn <- errno:
	default:
	}
	return
}

func (c *Conn) InData(ft *FrameData) (err error) {
	atomic.AddInt32(&c.rbufsize, int32(len(ft.Data)))
	c.rqueue.Push(ft.Data)
	log.Debug("%s recved %d bytes, rbufsize is %d bytes.",
		c.String(), len(ft.Data), atomic.LoadInt32(&c.rbufsize))
	return
}

func (c *Conn) InWnd(ft *FrameWnd) (err error) {
	atomic.AddInt32(&c.wbufsize, -int32(ft.Window))
	if atomic.LoadInt32(&c.wbufsize) < 0 {
		panic("wbufsize < 0")
	}
	c.wev.Signal()
	log.Debug("%s remote readed %d, write buffer size: %d.",
		c.String(), ft.Window, atomic.LoadInt32(&c.wbufsize))
	return nil
}

func (c *Conn) InFin(ft *FrameFin) (err error) {
	// always need to close read pipe
	// coz fin means remote will never send data anymore
	c.rqueue.Close()

	c.lock.Lock()

	switch c.status {
	case ST_EST:
		// close read pipe but not sent fin back
		// wait reader to close
		c.status = ST_CLOSE_WAIT
		c.lock.Unlock()
		log.Info("%s closed from remote.", c.String())
		return
	case ST_FIN_WAIT:
		c.lock.Unlock()
		// actually we don't need to *REALLY* wait 2MSL
		// because tcp will make sure fin arrival
		// don't need last ack or time wait to make sure that last ack will be received
		go c.Final()
		// in final rqueue.close will be call again, that's ok
		return
	default: // error
		c.lock.Unlock()
		log.Error("unknown status")
		return ErrFinState
	}
	return
}

func (c *Conn) CloseFrame() error {
	return c.InFin(nil)
}

func (c *Conn) Read(data []byte) (n int, err error) {
	var v interface{}
	c.rlock.Lock()
	defer c.rlock.Unlock()

	if len(data) > (1 << 30) {
		panic("read buf size too large")
	}

	target := data[:]
	for len(target) > 0 {
		if c.r_rest == nil {
			// reader should be blocked in here
			v, err = c.rqueue.Pop(n == 0)
			if err == ErrQueueClosed {
				err = io.EOF
			}
			if err != nil {
				return
			}

			if v == nil {
				break
			}
			c.r_rest = v.([]byte)
		}

		size := copy(target, c.r_rest)
		target = target[size:]
		n += size

		if len(c.r_rest) > size {
			c.r_rest = c.r_rest[size:]
		} else {
			// take all data in rest
			c.r_rest = nil
		}
	}

	atomic.AddInt32(&c.rbufsize, -int32(n))
	if atomic.LoadInt32(&c.rbufsize) < 0 {
		panic("rbufsize < 0")
	}

	fb := NewFrameWnd(c.streamid, uint32(n))
	err = c.sender.SendFrame(fb)
	if err != nil {
		log.Error("%s", err)
	}
	return
}

func (c *Conn) Write(data []byte) (n int, err error) {
	c.wlock.Lock()
	defer c.wlock.Unlock()

	for len(data) > 0 {
		size := uint32(len(data))
		// random size
		switch {
		case size > 8*1024:
			size = uint32(3*1024 + rand.Intn(1024))
		case 4*1024 < size && size <= 8*1024:
			size /= 2
		}

		err = c.writeSlice(data[:size])

		if err != nil {
			log.Error("%s", err)
			return
		}
		log.Debug("%s send chunk size %d at %d.", c.String(), size, n)

		data = data[size:]
		n += int(size)
	}
	log.Info("%s sent %d bytes.", c.String(), n)
	return
}

func (c *Conn) writeSlice(data []byte) (err error) {
	f := NewFrameData(c.streamid, data)

	if c.status != ST_EST && c.status != ST_CLOSE_WAIT {
		log.Error("status %d found in write slice", c.status)
		return ErrState
	}

	log.Debug("write buffer size: %d, write len: %d",
		atomic.LoadInt32(&c.wbufsize), len(data))
	for atomic.LoadInt32(&c.wbufsize)+int32(len(data)) > WINDOWSIZE {
		// this may cause block. maybe signal will be lost.
		c.wev.Wait()
	}

	err = c.sender.SendFrame(f)
	if err != nil {
		log.Info("%s", err)
		return
	}
	atomic.AddInt32(&c.wbufsize, int32(len(data)))
	c.wev.Signal()
	return
}

func (c *Conn) LocalAddr() net.Addr {
	return &Addr{
		c.sess.LocalAddr(),
		c.streamid,
	}
}

func (c *Conn) RemoteAddr() net.Addr {
	return &Addr{
		c.sess.RemoteAddr(),
		c.streamid,
	}
}

func (c *Conn) GetStatusString() (st string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	switch c.status {
	case ST_SYN_RECV:
		return "SYN_RECV"
	case ST_SYN_SENT:
		return "SYN_SENT"
	case ST_EST:
		return "ESTAB"
	case ST_CLOSE_WAIT:
		return "CLOSE_WAIT"
	case ST_FIN_WAIT:
		return "FIN_WAIT"
	}
	return "UNKNOWN"
}

func (c *Conn) CheckAndSetStatus(old uint8, new uint8) (err error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.status != old {
		err = ErrState
		log.Error("%s", err.Error())
		return
	}
	c.status = new
	return
}

func (c *Conn) GetReadBufSize() (n int32) {
	n = atomic.LoadInt32(&c.rbufsize)
	if n < 0 {
		panic("rbufsize < 0")
	}
	return
}

func (c *Conn) GetWriteBufSize() (n int32) {
	n = atomic.LoadInt32(&c.wbufsize)
	if n < 0 {
		panic("wbufsize < 0")
	}
	return
}

func (c *Conn) SetDeadline(t time.Time) error {
	return nil
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	return nil
}

type Addr struct {
	net.Addr
	streamid uint16
}

func (a *Addr) String() (s string) {
	return fmt.Sprintf("%s:%d", a.Addr.String(), a.streamid)
}
