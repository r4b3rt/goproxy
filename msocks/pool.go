package msocks

import (
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/shell909090/goproxy/sutils"
)

type SessionFactory struct {
	sutils.Dialer
	serveraddr string
	username   string
	password   string
}

func (sf *SessionFactory) CreateSession() (s *Session, err error) {
	log.Notice("msocks try to connect %s.", sf.serveraddr)

	conn, err := sf.Dialer.Dial("tcp", sf.serveraddr)
	if err != nil {
		return
	}

	ti := time.AfterFunc(AUTH_TIMEOUT*time.Second, func() {
		log.Notice(ErrAuthFailed.Error(), conn.RemoteAddr())
		conn.Close()
	})
	defer func() {
		ti.Stop()
	}()

	log.Notice("auth with username: %s, password: %s.", sf.username, sf.password)
	fb := NewFrameAuth(0, sf.username, sf.password)
	buf, err := fb.Packed()
	if err != nil {
		return
	}

	_, err = conn.Write(buf.Bytes())
	if err != nil {
		return
	}

	f, err := ReadFrame(conn)
	if err != nil {
		return
	}

	ft, ok := f.(*FrameResult)
	if !ok {
		return nil, ErrUnexpectedPkg
	}

	if ft.Errno != ERR_NONE {
		conn.Close()
		return nil, fmt.Errorf("create connection failed with code: %d.", ft.Errno)
	}

	log.Notice("auth passwd.")
	s = NewSession(conn)
	// s.pong()
	return
}

type SessionPool struct {
	mu      sync.Mutex // sess pool locker
	muf     sync.Mutex // factory locker
	sess    map[*Session]struct{}
	asfs    []*SessionFactory
	MinSess int
	MaxConn int
}

func CreateSessionPool(MinSess, MaxConn int) (sp *SessionPool) {
	if MinSess == 0 {
		MinSess = 1
	}
	if MaxConn == 0 {
		MaxConn = 16
	}
	sp = &SessionPool{
		sess:    make(map[*Session]struct{}, 0),
		MinSess: MinSess,
		MaxConn: MaxConn,
	}
	return
}

func (sp *SessionPool) AddSessionFactory(dialer sutils.Dialer, serveraddr, username, password string) {
	sf := &SessionFactory{
		Dialer:     dialer,
		serveraddr: serveraddr,
		username:   username,
		password:   password,
	}

	sp.muf.Lock()
	defer sp.muf.Unlock()
	sp.asfs = append(sp.asfs, sf)
}

func (sp *SessionPool) CutAll() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	for s, _ := range sp.sess {
		s.Close()
	}
	sp.sess = make(map[*Session]struct{}, 0)
}

func (sp *SessionPool) GetSize() int {
	return len(sp.sess)
}

func (sp *SessionPool) GetSessions() (sess map[*Session]struct{}) {
	return sp.sess
}

func (sp *SessionPool) Add(s *Session) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.sess[s] = struct{}{}
}

func (sp *SessionPool) Remove(s *Session) (err error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if _, ok := sp.sess[s]; !ok {
		return ErrSessionNotFound
	}
	delete(sp.sess, s)
	return
}

func (sp *SessionPool) Get() (sess *Session, err error) {
	if len(sp.sess) == 0 {
		err = sp.createSession(func() bool {
			return len(sp.sess) == 0
		})
		if err != nil {
			return nil, err
		}
	}

	sess, size := sp.getLessSess()
	if sess == nil {
		return nil, ErrNoSession
	}

	if size > sp.MaxConn || len(sp.sess) < sp.MinSess {
		go sp.createSession(func() bool {
			if len(sp.sess) < sp.MinSess {
				return true
			}
			// normally, size == -1 should never happen
			_, size := sp.getLessSess()
			return size > sp.MaxConn
		})
	}
	return
}

// Randomly select a server, try to connect with it. If it is failed, try next.
// Repeat for DIAL_RETRY times.
// Each time it will take 2 ^ (net.ipv4.tcp_syn_retries + 1) - 1 second(s).
// eg. net.ipv4.tcp_syn_retries = 4, connect will timeout in 2 ^ (4 + 1) -1 = 31s.
func (sp *SessionPool) createSession(checker func() bool) (err error) {
	sp.muf.Lock()
	defer sp.muf.Unlock()

	if checker != nil && !checker() {
		return
	}

	var sess *Session

	start := rand.Int()
	end := start + DIAL_RETRY*len(sp.asfs)
	for i := start; i < end; i++ {
		asf := sp.asfs[i%len(sp.asfs)]
		sess, err = asf.CreateSession()
		if err != nil {
			log.Error("%s", err)
			continue
		}
		break
	}

	if err != nil {
		log.Critical("can't connect to any server, quit.")
		return
	}
	log.Notice("session created.")

	sp.Add(sess)
	go sp.sessRun(sess)
	return
}

func (sp *SessionPool) getLessSess() (sess *Session, size int) {
	size = -1
	for s, _ := range sp.sess {
		if size == -1 || s.GetSize() < size {
			sess = s
			size = s.GetSize()
		}
	}
	return
}

func (sp *SessionPool) sessRun(sess *Session) {
	defer func() {
		err := sp.Remove(sess)
		if err != nil {
			log.Error("%s", err)
			return
		}

		// if n < sp.MinSess && !sess.IsGameOver() {
		// 	sp.createSession(func() bool {
		// 		return len(sp.sess) < sp.MinSess
		// 	})
		// }

		// Don't need to check less session here.
		// Mostly, less sess counter in here will not more then the counter in GetOrCreateSess.
		// The only exception is that the closing session is the one and only one
		// lower then max_conn
		// but we can think that as over max_conn line just happened.
	}()

	sess.Run()
	// that's mean session is dead
	log.Warning("session runtime quit, reboot from connect.")
	return
}

func (sp *SessionPool) Dial(network, address string) (net.Conn, error) {
	sess, err := sp.Get()
	if err != nil {
		return nil, err
	}
	return sess.Dial(network, address)
}

func (sp *SessionPool) LookupIP(host string) (addrs []net.IP, err error) {
	sess, err := sp.Get()
	if err != nil {
		return
	}
	return sess.LookupIP(host)
}
