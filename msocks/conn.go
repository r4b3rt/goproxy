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
	ch       chan uint32
	Network  string
	Address  string

	rlock    sync.Mutex // this should used to block reader and reader, not writer
	rbufsize uint32
	r_rest   []byte
	rqueue   *Queue

	wlock    sync.RWMutex
	wbufsize uint32
	wev      *sync.Cond
}

func NewConn(status uint8, streamid uint16, sess *Session, network, address string) (c *Conn) {
	c = &Conn{
		status:   status,
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

func (c *Conn) WaitForConn() (err error) {
	c.ch = make(chan uint32, 0)

	fb := NewFrameSyn(c.streamid, c.Network, c.Address)
	err = c.sess.SendFrame(fb)
	if err != nil {
		log.Error("%s", err)
		c.Final()
		return
	}

	errno := RecvWithTimeout(c.ch, DIAL_TIMEOUT*time.Second)
	if errno != ERR_NONE {
		log.Error("remote connect %s failed for %d.", c.String(), errno)
		c.Final()
	} else {
		log.Notice("%s connected: %s => %s.", c.Network, c.String(), c.Address)
	}

	c.ch = nil
	return
}

func (c *Conn) Final() {
	c.rqueue.Close()

	err := c.sess.RemovePort(c.streamid)
	if err != nil {
		log.Error("%s", err)
	}

	log.Notice("%s final.", c.String())
	c.status = ST_UNKNOWN
	return
}

func (c *Conn) Close() (err error) {
	log.Info("close %s.", c.String())
	c.lock.Lock()
	defer c.lock.Unlock()

	switch c.status {
	case ST_UNKNOWN, ST_FIN_WAIT:
		// maybe call close twice
		return
	case ST_EST:
		log.Info("%s closed from local.", c.String())
		fb := NewFrameFin(c.streamid)
		err = c.sender.SendFrame(fb)
		if err != nil {
			log.Error("%s", err)
			return
		}
		c.status = ST_FIN_WAIT
	case ST_CLOSE_WAIT:
		fb := NewFrameFin(c.streamid)
		err = c.sender.SendFrame(fb)
		if err != nil {
			log.Error("%s", err)
			return
		}
		c.Final()
	default:
		log.Error("%s", ErrUnknownState.Error())
	}

	return
}

func (c *Conn) SendFrame(f Frame) (err error) {
	switch ft := f.(type) {
	default:
		err = ErrUnexpectedPkg
		log.Error("%s", err)
		c.Close()
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
		c.Final()
	}
	return
}

func (c *Conn) InConnect(errno uint32) (err error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.status != ST_SYN_SENT {
		return ErrNotSyn
	}

	if errno == ERR_NONE {
		c.status = ST_EST
	} else {
		c.Final()
	}

	select {
	case c.ch <- errno:
	default:
	}
	return
}

func (c *Conn) InData(ft *FrameData) (err error) {
	log.Info("%s recved %d bytes.", c.String(), len(ft.Data))
	err = c.rqueue.Push(ft.Data)
	if err != nil {
		return
	}
	atomic.AddUint32(&c.rbufsize, uint32(len(ft.Data)))
	return
}

func (c *Conn) InWnd(ft *FrameWnd) (err error) {
	atomic.AddUint32(&c.wbufsize, -ft.Window)
	c.wev.Signal()
	log.Debug("remote readed %d, write buffer size: %d.",
		ft.Window, atomic.LoadUint32(&c.wbufsize))
	return nil
}

func (c *Conn) InFin(ft *FrameFin) (err error) {
	// always need to close read pipe
	// coz fin means remote will never send data anymore
	c.rqueue.Close()

	c.lock.Lock()
	defer c.lock.Unlock()

	switch c.status {
	case ST_EST:
		log.Info("%s closed from remote.", c.String())
		// close read pipe but not sent fin back
		// wait reader to close
		c.status = ST_CLOSE_WAIT
		return
	case ST_FIN_WAIT:
		// actually we don't need to *REALLY* wait 2MSL
		// because tcp will make sure fin arrival
		// don't need last ack or time wait to make sure that last ack will be received
		c.Final()
		// in final rqueue.close will be call again, that's ok
		return
	}
	// error
	return ErrFinState
}

func (c *Conn) CloseFrame() error {
	// maybe conn closed
	c.rqueue.Close()
	return nil
}

func (c *Conn) Read(data []byte) (n int, err error) {
	var v interface{}
	c.rlock.Lock()
	defer c.rlock.Unlock()

	target := data[:]
	block := true
	for len(target) > 0 {
		if c.r_rest == nil {
			// reader should be blocked in here
			v, err = c.rqueue.Pop(block)
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
		block = false

		if len(c.r_rest) > size {
			c.r_rest = c.r_rest[size:]
		} else {
			// take all data in rest
			c.r_rest = nil
		}
	}

	atomic.AddUint32(&c.rbufsize, -uint32(n))
	// c.rbufsize -= uint32(n)
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
		atomic.LoadUint32(&c.wbufsize), len(data))
	for atomic.LoadUint32(&c.wbufsize)+uint32(len(data)) > WINDOWSIZE {
		c.wev.Wait()
	}

	err = c.sender.SendFrame(f)
	if err != nil {
		log.Error("%s", err)
		return
	}
	atomic.AddUint32(&c.wbufsize, uint32(len(data)))
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

func (c *Conn) GetStatus() (st string) {
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

func (c *Conn) GetReadBufSize() (n uint32) {
	return atomic.LoadUint32(&c.rbufsize)
}

func (c *Conn) GetWriteBufSize() (n uint32) {
	return atomic.LoadUint32(&c.wbufsize)
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
