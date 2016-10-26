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
	return
}

type SessionPool struct {
	sess_lock      sync.RWMutex // sess pool locker
	factory_lock     sync.Mutex // factory locker
	sess    map[*Session]struct{}
	factories    []*SessionFactory
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

	sp.factory_lock.Lock()
	defer sp.factory_lock.Unlock()
	sp.factories = append(sp.factories, sf)
}

func (sp *SessionPool) CutAll() {
	sp.sess_lock.Lock()
	defer sp.sess_lock.Unlock()
	for s, _ := range sp.sess {
		s.Close()
	}
	sp.sess = make(map[*Session]struct{}, 0)
}

func (sp *SessionPool) GetSize() int {
	sp.sess_lock.RLock()
	defer sp.sess_lock.RUnlock()
	return len(sp.sess)
}

func (sp *SessionPool) GetSessions() (sessions []*Session) {
	sp.sess_lock.RLock()
	defer sp.sess_lock.RUnlock()
	for sess, _ := range sp.sess {
		sessions = append(sessions, sess)
	}
	return
}

func (sp *SessionPool) Add(s *Session) {
	sp.sess_lock.Lock()
	defer sp.sess_lock.Unlock()
	sp.sess[s] = struct{}{}
}

func (sp *SessionPool) Remove(s *Session) (err error) {
	sp.sess_lock.Lock()
	defer sp.sess_lock.Unlock()
	if _, ok := sp.sess[s]; !ok {
		return ErrSessionNotFound
	}
	delete(sp.sess, s)
	return
}

func (sp *SessionPool) Get() (sess *Session, err error) {
	sess_len := sp.GetSize()

	if sess_len == 0 {
		err = sp.createSession(func() bool {
			return sp.GetSize() == 0
		})
		if err != nil {
			return nil, err
		}
		sess_len = sp.GetSize()
	}

	sess, size := sp.getMinimumSess()
	if sess == nil {
		return nil, ErrNoSession
	}

	if size > sp.MaxConn || sess_len < sp.MinSess {
		go sp.createSession(func() bool {
			if sp.GetSize() < sp.MinSess {
				return true
			}
			// normally, size == -1 should never happen
			_, size := sp.getMinimumSess()
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
	var sess *Session
	sp.factory_lock.Lock()

	if checker != nil && !checker() {
		sp.factory_lock.Unlock()
		return
	}

	start := rand.Int()
	end := start + DIAL_RETRY*len(sp.factories)
	for i := start; i < end; i++ {
		asf := sp.factories[i%len(sp.factories)]
		sess, err = asf.CreateSession()
		if err != nil {
			log.Error("%s", err)
			continue
		}
		break
	}
	sp.factory_lock.Unlock()

	if err != nil {
		log.Critical("can't connect to any server, quit.")
		return
	}
	log.Notice("session created.")

	sp.Add(sess)
	go sp.sessRun(sess)
	return
}

func (sp *SessionPool) getMinimumSess() (sess *Session, size int) {
	size = -1
	sp.sess_lock.RLock()
	defer sp.sess_lock.RUnlock()
	for s, _ := range sp.sess {
		ssize := s.GetSize()
		if size == -1 || ssize < size {
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
