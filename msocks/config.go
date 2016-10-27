package msocks

import (
	"errors"
	"math/rand"
	"time"

	logging "github.com/op/go-logging"
)

const (
	DIAL_RETRY    = 2
	DIAL_TIMEOUT  = 30
	AUTH_TIMEOUT  = 10
	DNS_TIMEOUT   = 30
	WRITE_TIMEOUT = 60

	WINDOWSIZE = 4 * 1024 * 1024

	SHRINK_TIME = 3
	DEBUGDNS    = false
)

const (
	ERR_NONE = iota
	ERR_AUTH
	ERR_IDEXIST
	ERR_CONNFAILED
	ERR_TIMEOUT
	ERR_CLOSED
)

var (
	ErrNoSession       = errors.New("session in pool but can't pick one.")
	ErrSessionNotFound = errors.New("session not found.")
	ErrAuthFailed      = errors.New("auth failed.")
	ErrAuthTimeout     = errors.New("auth timeout %s.")
	ErrStreamNotExist  = errors.New("stream not exist.")
	ErrQueueClosed     = errors.New("queue closed.")
	ErrUnexpectedPkg   = errors.New("unexpected package.")
	ErrNotSyn          = errors.New("frame result in conn which status is not syn.")
	ErrFinState        = errors.New("status not est or fin wait when get fin.")
	ErrIdExist         = errors.New("frame sync stream id exist.")
	ErrState           = errors.New("status error.")
	ErrUnknownState    = errors.New("unknown status.")
	ErrChanClosed      = errors.New("chan closed.")
	ErrDnsTimeOut      = errors.New("dns timeout.")
	ErrDnsMsgIllegal   = errors.New("dns message illegal.")
	ErrNoDnsServer     = errors.New("no proper dns server.")
)

var (
	log = logging.MustGetLogger("msocks")
)

func init() {
	rand.Seed(time.Now().UnixNano())
}
