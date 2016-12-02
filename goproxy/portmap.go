package main

import (
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shell909090/goproxy/sutils"
)

const (
	UDP_TICK           = 60
	UDP_TIMEOUT        = 5
	UDP_BLOCK_INTERVAL = 500
	UDP_READBUFFER     = 1048576
)

type UdpPortMapper struct {
	lock  sync.Mutex
	ports map[net.Addr]*UdpMapperConn
}

func NewUdpPortMapper() (upm *UdpPortMapper) {
	upm = &UdpPortMapper{
		ports: make(map[net.Addr]*UdpMapperConn, 0),
	}
	return
}

func (upm *UdpPortMapper) RemovePorts(addr net.Addr) {
	upm.lock.Lock()
	defer upm.lock.Unlock()

	_, ok := upm.ports[addr]
	if !ok {
		logger.Error("remove a port not exits: %s.", addr.String())
		return
	}
	delete(upm.ports, addr)
	logger.Debug("remove port %s.", addr.String())
	return
}

func (upm *UdpPortMapper) UdpPortmap(pm PortMap, dialer sutils.Dialer) (err error) {
	laddr, err := net.ResolveUDPAddr(pm.Net, pm.Src)
	if err != nil {
		return
	}
	sconn, err := net.ListenUDP(pm.Net, laddr)
	if err != nil {
		return
	}
	defer sconn.Close()
	sconn.SetReadBuffer(UDP_READBUFFER)
	logger.Info("udp listening in %s", pm.Src)

	for {
		up := NewUdpPackage()
		nr, addr, err := sconn.ReadFrom(up.buf)
		switch err {
		case nil:
		case io.EOF:
			return nil
		default:
			logger.Error("%s", err.Error())
			continue
		}
		up.nr = nr

		upm.lock.Lock()
		umc, ok := upm.ports[addr]
		if !ok {
			logger.Info("udp forward got new addr %s.", addr)
			dconn, err := dialer.Dial(pm.Net, pm.Dst)
			if err != nil {
				upm.lock.Unlock()
				logger.Error("%s", err.Error())
				continue
			}
			umc = NewUdpMapperConn(upm, sconn, dconn, addr, pm.Dst)
			upm.ports[addr] = umc
			umc.Run()
		}
		upm.lock.Unlock()

		umc.ch <- up
	}
}

type UdpPackage struct {
	buf []byte
	nr  int
}

func NewUdpPackage() (up *UdpPackage) {
	up = &UdpPackage{
		buf: allocbuf(),
	}
	return
}

func (up *UdpPackage) Free() {
	freebuf(up.buf)
}

type UdpMapperConn struct {
	upm   *UdpPortMapper
	tick  <-chan time.Time
	cnt   int32
	sconn *net.UDPConn
	dconn net.Conn
	addr  net.Addr
	dst   string
	ch    chan *UdpPackage
}

func NewUdpMapperConn(upm *UdpPortMapper, sconn *net.UDPConn,
	dconn net.Conn, addr net.Addr, dst string) (umc *UdpMapperConn) {
	umc = &UdpMapperConn{
		upm:   upm,
		tick:  time.Tick(UDP_TICK * time.Second),
		sconn: sconn,
		dconn: dconn,
		addr:  addr,
		dst:   dst,
		ch:    make(chan *UdpPackage, 0),
	}
	return
}

func (umc *UdpMapperConn) Close() {
	logger.Notice("udp redirect %s closed.", umc.addr.String())
	umc.dconn.Close()
	close(umc.ch)
	umc.upm.RemovePorts(umc.addr)
	return
}

func (umc *UdpMapperConn) Run() {
	go umc.SendHandler()
	go umc.RecvHandler()
	go func() {
		for _ = range umc.tick {
			if atomic.AddInt32(&umc.cnt, 1) >= UDP_TIMEOUT {
				umc.Close()
				return
			}
		}
	}()
}

func (umc *UdpMapperConn) RecvHandler() {
	var buf [8192]byte
	defer umc.dconn.Close()
	for {
		nr, err := umc.dconn.Read(buf[:])
		switch err {
		case nil:
		case io.EOF:
			return
		default:
			logger.Error("%s", err.Error())
			continue
		}

		_, err = umc.sconn.WriteTo(buf[0:nr], umc.addr)
		switch err {
		case nil:
		case io.EOF:
			return
		default:
			logger.Error("%s", err.Error())
			continue
		}

		atomic.StoreInt32(&umc.cnt, 0)
		logger.Debug("udp package recved %s <=> %s.", umc.addr.String(), umc.dst)
	}
}

func (umc *UdpMapperConn) SendHandler() {
	defer umc.dconn.Close()
	for {
		up, ok := <-umc.ch
		if !ok {
			return
		}

		_, err := umc.dconn.Write(up.buf[0:up.nr])
		switch err {
		case nil:
		case io.EOF:
			return
		default:
			logger.Error("%s", err.Error())
			continue
		}
		up.Free()

		atomic.StoreInt32(&umc.cnt, 0)
		logger.Debug("udp package sent %s <=> %s.", umc.addr.String(), umc.dst)
	}
}

func TcpPortmap(pm PortMap, dialer sutils.Dialer) (err error) {
	lsock, err := net.Listen(pm.Net, pm.Src)
	if err != nil {
		return
	}
	logger.Info("tcp listening in %s", pm.Src)

	for {
		var sconn, dconn net.Conn

		sconn, err = lsock.Accept()
		if err != nil {
			continue
		}
		logger.Info("accept in %s:%s, try to dial %s.", pm.Net, pm.Src, pm.Dst)

		dconn, err = dialer.Dial(pm.Net, pm.Dst)
		if err != nil {
			sconn.Close()
			continue
		}

		go sutils.CopyLink(dconn, sconn)
	}
}

func CreatePortmap(pm PortMap, dialer sutils.Dialer) {
	var err error
	if strings.HasPrefix(pm.Net, "udp") {
		upm := NewUdpPortMapper()
		err = upm.UdpPortmap(pm, dialer)
	} else {
		err = TcpPortmap(pm, dialer)
	}
	if err != nil {
		logger.Error("%s", err.Error())
	}
	return
}
