package msocks

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/shell909090/goproxy/sutils"
)

type Session struct {
	wlock sync.Mutex
	conn  net.Conn

	closed  bool
	plock   sync.RWMutex
	next_id uint16
	ports   map[uint16]FrameSender

	dialer   sutils.Dialer
	Readcnt  *SpeedCounter
	Writecnt *SpeedCounter
}

func NewSession(conn net.Conn) (s *Session) {
	s = &Session{
		conn:     conn,
		closed:   false,
		ports:    make(map[uint16]FrameSender, 0),
		Readcnt:  NewSpeedCounter(),
		Writecnt: NewSpeedCounter(),
	}
	log.Notice("session %s created.", s.String())
	return
}

func (s *Session) String() string {
	return fmt.Sprintf("%d", s.LocalPort())
}

func (s *Session) GetSize() int {
	s.plock.RLock()
	defer s.plock.RUnlock()
	return len(s.ports)
}

func (s *Session) GetPorts() (ports []*Conn) {
	s.plock.RLock()
	defer s.plock.RUnlock()

	for _, fs := range s.ports {
		if c, ok := fs.(*Conn); ok {
			ports = append(ports, c)
		}
	}
	return
}

type ConnSlice []*Conn

func (cs ConnSlice) Len() int           { return len(cs) }
func (cs ConnSlice) Swap(i, j int)      { cs[i], cs[j] = cs[j], cs[i] }
func (cs ConnSlice) Less(i, j int) bool { return cs[i].streamid < cs[j].streamid }

func (s *Session) GetSortedPorts() (ports ConnSlice) {
	ports = s.GetPorts()
	sort.Sort(ports)
	return
}

func (s *Session) PutIntoNextId(fs FrameSender) (id uint16, err error) {
	s.plock.Lock()

	startid := s.next_id
	for _, ok := s.ports[s.next_id]; ok; _, ok = s.ports[s.next_id] {
		s.next_id += 1
		if s.next_id == startid {
			err = errors.New("run out of stream id")
			log.Error("%s", err)
			return
		}
	}
	id = s.next_id
	s.next_id += 2
	s.ports[id] = fs

	s.plock.Unlock()

	log.Debug("%s put into next id %d: %p.", s.String(), id, fs)
	return
}

func (s *Session) PutIntoId(id uint16, fs FrameSender) (err error) {
	log.Debug("%s put into id %d: %p.", s.String(), id, fs)

	s.plock.Lock()

	_, ok := s.ports[id]
	if ok {
		return ErrIdExist
	}
	s.ports[id] = fs

	s.plock.Unlock()
	return
}

func (s *Session) RemovePort(streamid uint16) (err error) {
	s.plock.Lock()
	_, ok := s.ports[streamid]
	if !ok {
		return fmt.Errorf("streamid(%d) not exist.", streamid)
	}
	delete(s.ports, streamid)
	s.plock.Unlock()

	log.Info("%s remove port %d.", s.String(), streamid)
	return
}

func (s *Session) Close() (err error) {
	defer s.conn.Close()

	err = s.Readcnt.Close()
	if err != nil {
		return
	}
	err = s.Writecnt.Close()
	if err != nil {
		return
	}

	s.plock.RLock()
	defer s.plock.RUnlock()

	log.Warning("close all connects (%d) for session: %s.",
		len(s.ports), s.String())

	for _, v := range s.ports {
		go v.CloseFrame()
	}
	s.closed = true
	return
}

func (s *Session) LocalAddr() net.Addr {
	return s.conn.LocalAddr()
}

func (s *Session) RemoteAddr() net.Addr {
	return s.conn.RemoteAddr()
}

func (s *Session) LocalPort() int {
	addr, ok := s.LocalAddr().(*net.TCPAddr)
	if !ok {
		return -1
	}
	return addr.Port
}

func (s *Session) SendFrame(f Frame) (err error) {
	log.Debug("sent %s", f.Debug())
	s.Writecnt.Add(uint32(f.GetSize() + 5))

	buf, err := f.Packed()
	if err != nil {
		return
	}
	b := buf.Bytes()

	s.wlock.Lock()
	s.conn.SetWriteDeadline(time.Now().Add(WRITE_TIMEOUT * time.Second))
	n, err := s.conn.Write(b)
	s.wlock.Unlock()
	if err != nil {
		return
	}
	if n != len(b) {
		return io.ErrShortWrite
	}
	log.Debug("sess %s write %d bytes.", s.String(), len(b))
	return
}

func (s *Session) CloseFrame() error {
	return s.Close()
}

func (s *Session) Run() {
	defer s.Close()

	for {
		f, err := ReadFrame(s.conn)
		if err != nil {
			log.Error("%s", err)
			return
		}

		log.Debug("recv %s", f.Debug())
		s.Readcnt.Add(uint32(f.GetSize() + 5))

		switch ft := f.(type) {
		default:
			log.Error("%s", ErrUnexpectedPkg.Error())
			return
		case *FrameResult, *FrameData, *FrameWnd, *FrameFin, *FrameRst:
			err = s.sendFrameInChan(f)
			if err != nil {
				log.Error("%s(%d) send failed, err: %s.",
					s.String(), f.GetStreamid(), err.Error())
				return
			}
		case *FrameSyn:
			err = s.on_syn(ft)
			if err != nil {
				log.Error("syn failed: %s", err.Error())
				return
			}
		case *FrameDns:
			err = s.on_dns(ft)
			if err != nil {
				log.Error("dns failed: %s", err.Error())
				return
			}
		case *FramePing:
		case *FrameSpam:
		}
	}
}

// no drop, any error will reset main connection
func (s *Session) sendFrameInChan(f Frame) (err error) {
	streamid := f.GetStreamid()

	s.plock.RLock()
	c, ok := s.ports[streamid]
	s.plock.RUnlock()
	if !ok || c == nil {
		return ErrStreamNotExist
	}

	err = c.SendFrame(f)
	if err != nil {
		return
	}
	return nil
}

// ---- syn part ----

func (s *Session) Dial(network, address string) (c *Conn, err error) {
	c = NewConn(0, s, network, address)
	streamid, err := s.PutIntoNextId(c)
	if err != nil {
		return
	}
	c.streamid = streamid

	log.Info("try dial %s => %s.", s.conn.RemoteAddr().String(), address)
	err = c.SendSynAndWait()
	if err != nil {
		return
	}

	return c, nil
}

func (s *Session) on_syn(ft *FrameSyn) (err error) {
	// lock streamid temporary, with status sync recved
	c := NewConn(ft.Streamid, s, ft.Network, ft.Address)
	err = c.CheckAndSetStatus(ST_UNKNOWN, ST_SYN_RECV)
	if err != nil {
		return
	}

	err = s.PutIntoId(ft.Streamid, c)
	if err != nil {
		log.Error("%s", err.Error())

		fb := NewFrameResult(ft.Streamid, ERR_IDEXIST)
		err = s.SendFrame(fb)
		if err != nil {
			log.Error("%s", err.Error())
		}
		return
	}

	// it may toke long time to connect with target address
	// so we use goroutine to return back loop
	go func() {
		var err error
		var conn net.Conn
		log.Debug("try to connect %s => %s:%s.", c.String(), ft.Network, ft.Address)

		if dialer, ok := s.dialer.(*sutils.TcpDialer); ok {
			conn, err = dialer.DialTimeout(ft.Network, ft.Address, DIAL_TIMEOUT*time.Second)
		} else {
			conn, err = s.dialer.Dial(ft.Network, ft.Address)
		}

		if err != nil {
			log.Error("%s", err)
			fb := NewFrameResult(ft.Streamid, ERR_CONNFAILED)
			err = s.SendFrame(fb)
			if err != nil {
				log.Error("%s", err)
			}
			c.Final()
			return
		}

		fb := NewFrameResult(ft.Streamid, ERR_NONE)
		err = s.SendFrame(fb)
		if err != nil {
			log.Error("%s", err)
			return
		}
		err = c.CheckAndSetStatus(ST_SYN_RECV, ST_EST)
		if err != nil {
			return
		}

		go sutils.CopyLink(conn, c)
		log.Notice("connected %s => %s:%s.", c.String(), ft.Network, ft.Address)
		return
	}()
	return
}

// ---- syn part ended ----

// ---- dns part ----

func MakeDnsFrame(host string, t uint16, streamid uint16) (req *dns.Msg, f Frame, err error) {
	log.Debug("make a dns query for %s.", host)

	req = new(dns.Msg)
	req.Id = dns.Id()
	req.SetQuestion(dns.Fqdn(host), t)
	req.RecursionDesired = true

	b, err := req.Pack()
	if err != nil {
		return
	}

	f = NewFrameDns(streamid, b)
	return
}

func DebugDNS(r *dns.Msg, name string) {
	straddr := ""
	for _, a := range r.Answer {
		switch ta := a.(type) {
		case *dns.A:
			straddr += ta.A.String() + ","
		case *dns.AAAA:
			straddr += ta.AAAA.String() + ","
		}
	}
	log.Info("dns result for %s is %s.", name, straddr)
	return
}

func ParseDnsFrame(f Frame, req *dns.Msg) (addrs []net.IP, err error) {
	ft, ok := f.(*FrameDns)
	if !ok {
		return nil, ErrDnsMsgIllegal
	}

	res := new(dns.Msg)
	err = res.Unpack(ft.Data)
	if err != nil || !res.Response || res.Id != req.Id {
		return nil, ErrDnsMsgIllegal
	}

	if DEBUGDNS {
		DebugDNS(res, req.Question[0].Name)
	}
	for _, a := range res.Answer {
		switch ta := a.(type) {
		case *dns.A:
			addrs = append(addrs, ta.A)
		case *dns.AAAA:
			addrs = append(addrs, ta.AAAA)
		}
	}
	return
}

func (s *Session) LookupIP(host string) (addrs []net.IP, err error) {
	ip := net.ParseIP(host)
	if ip != nil {
		return []net.IP{ip}, nil
	}

	cfs := CreateChanFrameSender(0)
	streamid, err := s.PutIntoNextId(&cfs)
	if err != nil {
		return
	}
	defer func() {
		err := s.RemovePort(streamid)
		if err != nil {
			log.Error("%s", err.Error())
		}
	}()

	req, freq, err := MakeDnsFrame(host, dns.TypeA, streamid)
	if err != nil {
		return
	}

	err = s.SendFrame(freq)
	if err != nil {
		return
	}

	fres, err := cfs.RecvWithTimeout(DNS_TIMEOUT * time.Second)
	if err != nil {
		return
	}

	addrs, err = ParseDnsFrame(fres, req)
	return
}

func (s *Session) on_dns(ft *FrameDns) (err error) {
	req := new(dns.Msg)
	err = req.Unpack(ft.Data)
	if err != nil {
		return ErrDnsMsgIllegal
	}

	if req.Response {
		// ignore send fail, maybe just timeout.
		// should I log this ?
		return s.sendFrameInChan(ft)
	}

	log.Info("dns query for %s.", req.Question[0].Name)

	d, ok := sutils.DefaultLookuper.(*sutils.DnsLookup)
	if !ok {
		return ErrNoDnsServer
	}
	res, err := d.Exchange(req)
	if err != nil {
		log.Error("%s", err.Error())
		return nil
	}

	if DEBUGDNS {
		DebugDNS(res, req.Question[0].Name)
	}

	// send response back from streamid
	b, err := res.Pack()
	if err != nil {
		log.Error("%s", ErrDnsMsgIllegal.Error())
		return nil
	}

	fr := NewFrameDns(ft.GetStreamid(), b)
	err = s.SendFrame(fr)
	return
}

// ---- dns part ended ----
