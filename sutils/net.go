package sutils

import (
	"net"
	"time"

	"github.com/miekg/dns"
)

type Dialer interface {
	Dial(string, string) (net.Conn, error)
}

type TcpDialer struct {
}

func (td *TcpDialer) Dial(network, address string) (net.Conn, error) {
	return net.Dial(network, address)
}

func (td *TcpDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout(network, address, timeout)
}

var DefaultTcpDialer = &TcpDialer{}

type Tcp4Dialer struct {
}

func (td *Tcp4Dialer) Dial(network, address string) (net.Conn, error) {
	return net.Dial("tcp4", address)
}

func (td *Tcp4Dialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp4", address, timeout)
}

var DefaultTcp4Dialer = &Tcp4Dialer{}

type Lookuper interface {
	LookupIP(host string) (addrs []net.IP, err error)
}

type NetLookupIP struct {
}

func (n *NetLookupIP) LookupIP(host string) (addrs []net.IP, err error) {
	return net.LookupIP(host)
}

var DefaultLookuper Lookuper

func init() {
	conf, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil {
		return
	}

	var addrs []string
	for _, srv := range conf.Servers {
		addrs = append(addrs, net.JoinHostPort(srv, conf.Port))
	}

	DefaultLookuper = NewDnsLookup(addrs, "")
}
