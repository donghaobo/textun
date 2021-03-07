package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
)

func writeDone(w io.Writer, p []byte) error {
	for {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == len(p) {
			break
		}
		p = p[n:]
	}
	return nil
}
func readDone(r io.Reader, p []byte) error {
	for {
		n, err := r.Read(p)
		if err != nil {
			return err
		}
		if n == len(p) {
			return nil
		}
		p = p[n:]
	}
}

const PREFIXLEN = 8

type LineMux struct {
	route map[string]*LineChan
	rl    sync.RWMutex
	base  io.ReadWriter
	lineR *bufio.Reader
	dft   io.ReadWriter
	buf   *bytes.Buffer
	err   error
	rbuf  []byte
	rc    chan []byte
	rerr  chan error
	lock  sync.Mutex
}
type LineChan struct {
	prefix string
	dc     chan []byte
	ec     chan error
	lm     *LineMux
	rbuf   []byte
}

func (lc *LineChan) String() string {
	return fmt.Sprintf("lineChan:prefix:'%s'", lc.prefix)
}
func (lc *LineChan) Read(b []byte) (n int, err error) {
	tlog.Printf("LineChan trying to read %d bytes", len(b))
	var buf []byte
	if len(lc.rbuf) != 0 {
		buf = lc.rbuf
		goto copybuf
	}
	select {
	case buf = <-lc.dc:
	case err = <-lc.ec:
		return 0, err
	}
copybuf:
	cn := copy(b, buf)
	if cn != len(buf) {
		lc.rbuf = buf[cn:]
	}
	return cn, nil
}
func (lc *LineChan) Write(b []byte) (n int, err error) {
	tlog.Printf("LineChan trying to write %d bytes", len(b))
	lc.lm.lock.Lock()
	defer lc.lm.lock.Unlock()
	buf := make([]byte, len(b)+len(lc.prefix))
	copy(buf, []byte(lc.prefix))
	copy(buf[len(lc.prefix):], b)
	err = writeDone(lc.lm, buf)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}
func (lc *LineChan) Close() error {
	lc.lm.rl.Lock()
	delete(lc.lm.route, lc.prefix)
	lc.lm.rl.Unlock()
	return nil
}
func (lm *LineMux) String() string {
	lm.rl.RLock()
	defer lm.rl.RUnlock()
	return fmt.Sprintf("lineMux:{base:%+v,route%+v}", lm.base, lm.route)
}

func (lm *LineMux) Write(b []byte) (n int, err error) {
	tlog.Printf("LineMux trying to write %d bytes ==========\n%s\n=============", len(b), string(b))
	return lm.base.Write(b)
}

func NewLineMux(base io.ReadWriter) *LineMux {
	ts := &LineMux{
		route: make(map[string]*LineChan),
		base:  base,
		lineR: bufio.NewReaderSize(base, 4096*4096),
		buf:   &bytes.Buffer{},
		err:   nil,
		rbuf:  []byte{},
		rc:    make(chan []byte),
		rerr:  make(chan error),
	}
	go ts.poll()
	return ts
}
func (lm *LineMux) poll() {
	tlog.Printf("LineMux %v start polling", lm)
	var err error
	for {
		line := []byte{}
		tlog.Printf("LineMux %v trying to get line", lm)
		line, err = lm.lineR.ReadBytes('\n')
		if err != nil {
			break
		}
		tlog.Printf("LineMux %v got line ======\n%s\n======", lm, line)
		prefix := make([]byte, PREFIXLEN)
		copy(prefix, line)
		lm.rl.RLock()
		lc, ok := lm.route[string(prefix)]
		if !ok || len(line) < PREFIXLEN {
			lc, ok = lm.route[""]
			if ok {
				tlog.Printf("LineMux %v route line to %s", lm, lc)
				select {
				case lc.dc <- line:
				default:
					// run unblocked, will retrans
				}
			}
		} else {
			tlog.Printf("LineMux %v route line to %s", lm, lc)
			select {
			case lc.dc <- line[PREFIXLEN:]:
			default:
				// run unblocked, will retrans
			}
		}
		lm.rl.RUnlock()
	}
	lm.rl.RLock()
	for _, c := range lm.route {
		c.ec <- err
	}
	lm.rl.RUnlock()
}
func (lm *LineMux) NewChannel(prefix string) (c *LineChan, err error) {
	if len(prefix) != PREFIXLEN && prefix != "" {
		return nil, errors.New(fmt.Sprintf(`prefix '%s' MUST be %d bytes, or ""`, prefix, PREFIXLEN))
	}
	lm.rl.RLock()
	_, ok := lm.route[prefix]
	lm.rl.RUnlock()
	if ok {
		return nil, errors.New(fmt.Sprintf("channel exist for prefix:'%s'", prefix))
	}
	lc := &LineChan{
		prefix: prefix,
		dc:     make(chan []byte, 1000),
		ec:     make(chan error, 10),
		lm:     lm,
	}
	lm.rl.Lock()
	lm.route[prefix] = lc
	lm.rl.Unlock()
	return lc, nil
}
