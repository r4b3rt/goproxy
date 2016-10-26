### Makefile --- 

## Author: shell@shell-deb.shdiv.qizhitech.com
## Version: $Id: Makefile,v 0.0 2012/11/02 06:18:14 shell Exp $
## Keywords: 
## X-URL: 
LEVEL=NOTICE

all: build

buildtar: build
	strip bin/goproxy
	tar cJf ../goproxy-`uname -m`.tar.xz bin/goproxy debian/config.json debian/routes.list.gz

clean:
	rm -rf bin pkg
	debclean

test:
	go test github.com/shell909090/goproxy/ipfilter
	go test github.com/shell909090/goproxy/msocks

install-dep:
	go get github.com/shell909090/goproxy
	go get github.com/op/go-logging
	go get github.com/miekg/dns

build:
	mkdir -p bin
	go build -race -o bin/goproxy github.com/shell909090/goproxy/goproxy

install: build
	install -d $(DESTDIR)/usr/bin/
	install -m 755 -s bin/goproxy $(DESTDIR)/usr/bin/
	install -d $(DESTDIR)/usr/share/goproxy/
	install -m 644 debian/routes.list.gz $(DESTDIR)/usr/share/goproxy/
	install -d $(DESTDIR)/etc/goproxy/
	install -m 644 debian/config.json $(DESTDIR)/etc/goproxy/

press-clean:
	rm -f server.log client.log httproxy.log

press: build press-clean
	bin/goproxy -config=server.json &
	bin/goproxy -config=client.json &
	sleep 1
	# curl -x http://localhost:5234 http://localhost:6060/ > /dev/null
	curl -x http://localhost:5234 http://www.microsoft.com > /dev/null
	killall goproxy

### Makefile ends here
