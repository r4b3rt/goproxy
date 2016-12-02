package main

import (
	"net"
	"net/http"

	"github.com/shell909090/goproxy/cryptconn"
	"github.com/shell909090/goproxy/ipfilter"
	"github.com/shell909090/goproxy/msocks"
	"github.com/shell909090/goproxy/sutils"
)

func httpserver(addr string, handler http.Handler) {
	for {
		err := http.ListenAndServe(addr, handler)
		if err != nil {
			logger.Error("%s", err.Error())
			return
		}
	}
}

func run_server(basecfg *Config) (err error) {
	cfg, err := LoadServerConfig(basecfg)
	if err != nil {
		return
	}

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return
	}

	listener, err = cryptconn.NewListener(listener, cfg.Cipher, cfg.Key)
	if err != nil {
		return
	}

	var dialer sutils.Dialer = sutils.DefaultTcpDialer
	if cfg.ForceIPv4 {
		logger.Info("force ipv4 dailer.")
		dialer = sutils.DefaultTcp4Dialer
	}

	svr, err := msocks.NewServer(cfg.Auth, dialer)
	if err != nil {
		return
	}

	if cfg.AdminIface != "" {
		mux := http.NewServeMux()
		NewMsocksManager(svr.SessionPool).Register(mux)
		go httpserver(cfg.AdminIface, mux)
	}

	return svr.Serve(listener)
}

func run_httproxy(basecfg *Config) (err error) {
	cfg, err := LoadClientConfig(basecfg)
	if err != nil {
		return
	}

	var dialer sutils.Dialer
	sp := msocks.CreateSessionPool(cfg.MinSess, cfg.MaxConn)

	for _, srv := range cfg.Servers {
		cipher := srv.Cipher
		if cipher == "" {
			cipher = cfg.Cipher
		}
		dialer, err = cryptconn.NewDialer(sutils.DefaultTcpDialer, cipher, srv.Key)
		if err != nil {
			return
		}
		sp.AddSessionFactory(dialer, srv.Server, srv.Username, srv.Password)
	}

	dialer = sp

	if cfg.DnsNet == TypeInternal {
		sutils.DefaultLookuper = sp
	}

	if cfg.AdminIface != "" {
		mux := http.NewServeMux()
		NewMsocksManager(sp).Register(mux)
		go httpserver(cfg.AdminIface, mux)
	}

	if cfg.Blackfile != "" {
		fdialer := ipfilter.NewFilteredDialer(dialer)
		err = fdialer.LoadFilter(sutils.DefaultTcpDialer, cfg.Blackfile)
		if err != nil {
			logger.Error("%s", err.Error())
			return
		}
		dialer = fdialer
	}

	for _, pm := range cfg.Portmaps {
		go CreatePortmap(pm, dialer)
	}

	return http.ListenAndServe(cfg.Listen, NewProxy(dialer, cfg.HttpUser, cfg.HttpPassword))
}
