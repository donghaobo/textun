package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"
)

type TextSocket struct {
	io       io.ReadWriter
	mode     int
	channels map[uint16]*TextConn
	lastCid  uint16
	cc       chan uint16
	id       uint32
	lock     sync.Mutex
}

func (ts *TextSocket) String() string {
	return fmt.Sprintf("textSocket:mode%d:on%+v", ts.mode, ts.io)
}

const (
	STATDATA = iota
	STATCLOSE
)

type TextConn struct {
	cid  uint16
	c    chan []byte
	ec   chan error
	rbuf []byte
	ts   *TextSocket
	stat int
	seq  uint64
	rseq uint64
	wq   *pktq
	rq   *pktq
}

func (tc *TextConn) recv() error {
	tlog.Printf("%s recv pkts", tc)
restart:
	tp := tc.rq.first()
	if tp == nil {
		return nil
	}
	tlog.Printf("%s recv pkts, seq %d, rseq %d", tc, tp.seq, tc.rseq)
	if tp.seq != tc.rseq {
		return nil
	}
	select {
	case tc.c <- tp.data:
	default:
		return nil
	}
	tc.rq.delete(tp.seq)
	tc.rseq++
	goto restart
}
func (tc *TextConn) Read(b []byte) (n int, err error) {
	tlog.Printf("TextConn trying to read %d bytes", len(b))
	if tc.stat == STATCLOSE {
		return 0, io.EOF
	}
	var buf []byte
	if len(tc.rbuf) != 0 {
		buf = tc.rbuf
		goto copybuf
	}
	select {
	case buf = <-tc.c:
	case err = <-tc.ec:
		return 0, err
	}
copybuf:
	cn := copy(b, buf)
	if cn != len(buf) {
		tc.rbuf = buf[cn:]
	}
	return cn, nil
}

func (tc *TextConn) Write(b []byte) (n int, err error) {
	tlog.Printf("TextConn trying to write %d bytes", len(b))
	for {
		tc.ts.lock.Lock()
		if tc.wq.full() {
			tc.ts.lock.Unlock()
			//tlog.Printf("TextConn waiting for wq, %#v", tc.wq)
			time.Sleep(time.Millisecond * 20)
		} else {
			break
		}
	}
	defer tc.ts.lock.Unlock()
	if tc.stat == STATCLOSE {
		return 0, io.EOF
	}
	tp := &textPkt{
		t:       DATA,
		channel: tc.cid,
		id:      tc.ts.id,
		seq:     tc.seq,
		data:    []byte{},
	}
	tp.data = b
	err = tc.ts.send(tp)
	if err != nil {
		tc.Close()
		return 0, err
	}
	tc.seq++
	return len(b), nil
}

func (tc *TextConn) close() error {
	tlog.Printf("%s is sending close msg and delete self", tc)
	tp := &textPkt{
		t:       CLOSE,
		channel: tc.cid,
		id:      tc.ts.id,
		seq:     0,
		data:    []byte{},
	}
	tc.ts.send(tp)
	tc.ts.putChannel(tc)
	return nil
}
func (tc *TextConn) Close() error {
	tlog.Printf("%s is closing", tc)
	tc.ts.lock.Lock()
	defer tc.ts.lock.Unlock()
	return tc.close()
}
func (tc *TextConn) Network() string {
	return "text"
}
func (tc *TextConn) String() string {
	return fmt.Sprintf("textConn:channel:%d:on%+v", tc.cid, tc.ts)
}
func (tc *TextConn) LocalAddr() net.Addr {
	return tc
}

func (tc *TextConn) RemoteAddr() net.Addr {
	return tc
}

func (tc *TextConn) SetDeadline(t time.Time) error {
	return nil
}

func (tc *TextConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (tc *TextConn) SetWriteDeadline(t time.Time) error {
	return nil
}
func NewTextSocket(io io.ReadWriter) *TextSocket {
	ts := &TextSocket{
		io:       io,
		mode:     0,
		channels: make(map[uint16]*TextConn),
		lastCid:  0,
		cc:       make(chan uint16, 100),
		id:       0,
		lock:     sync.Mutex{},
	}
	// channel 0 is unused
	//ts.channels[0] = &TextConn{}
	go ts.poll()
	go ts.retrans()
	return ts
}
func (ts *TextSocket) Accept() (net.Conn, error) {
	if ts.mode != 0 && ts.mode != SERVER {
		return nil, errors.New("text socket is not server")
	}
	ts.mode = SERVER
	ts.id = SERVER
	cid := <-ts.cc
	ts.lock.Lock()
	defer ts.lock.Unlock()
	ts.channels[cid] = &TextConn{
		cid:  cid,
		c:    make(chan []byte, 100),
		ec:   make(chan error, 10),
		rbuf: []byte{},
		ts:   ts,
		wq:   newpktq(10),
		rq:   newpktq(100),
	}
	return ts.channels[cid], nil
}
func (ts *TextSocket) Connect() (net.Conn, error) {
	ts.lock.Lock()
	defer ts.lock.Unlock()
	if ts.mode != 0 && ts.mode != CLIENT {
		return nil, errors.New("text socket is not client")
	}
	ts.mode = CLIENT
	ts.id = CLIENT
	tc, err := ts.newChannel()
	if err != nil {
		return tc, err
	}

	tp := &textPkt{
		t:       CONNECT,
		channel: tc.cid,
		id:      ts.id,
		seq:     0,
		data:    []byte{},
	}
	err = ts.send(tp)
	if err != nil {
		tc.Close()
		return nil, err
	}
	// TODO wait ACK ??
	return tc, nil
}
func (ts *TextSocket) send(tp *textPkt) error {
	tlog.Printf("send pkt type %d channel:%d seq %d data len %d", tp.t, tp.channel, tp.seq, len(tp.data))
	tp.FillCrc()
	if tp.t == DATA && tp.retransed == 0 {
		tp.retrans = time.Now().Add(randretrans(retransMin))
		if tc, ok := ts.channels[tp.channel]; ok {
			tc.wq.add(tp)
		}
	}
	return writeDone(ts.io, tp.Bytes())
}
func (ts *TextSocket) newChannel() (*TextConn, error) {
	for cid := ts.lastCid + 1; cid != ts.lastCid; cid++ {
		if _, ok := ts.channels[cid]; !ok {
			ts.channels[cid] = &TextConn{
				cid:  cid,
				c:    make(chan []byte, 100),
				ec:   make(chan error, 10),
				rbuf: []byte{},
				ts:   ts,
				wq:   newpktq(10),
				rq:   newpktq(100),
			}
			ts.lastCid = cid
			return ts.channels[cid], nil
		}
	}
	return nil, errors.New("channel table full")
}
func (ts *TextSocket) putChannel(c *TextConn) error {
	if _, ok := ts.channels[c.cid]; c.cid != 0 && ok {
		delete(ts.channels, c.cid)
	}
	return nil
}

var retransMin = time.Millisecond * 20

func randretrans(t time.Duration) time.Duration {
	return retransMin * time.Duration(rand.Intn(200)) / time.Duration(100)
}
func (ts *TextSocket) retrans() {
	for {
		sleeptime := time.Second
		//tlog.Printf("Textsocket retrans started")
		ts.lock.Lock()
		//tlog.Printf("Textsocket retrans got lock")
		for cid, tc := range ts.channels {
			pkt := tc.wq.first()
			if pkt == nil {
				continue
			}
			if pkt.retransed > 10 {
				tlog.Printf("pkt retrans too much close force")
				tc.close()
				continue
			}
			//tlog.Printf("Textsocket retrans check for channel %d seq %d, now %s, retrans %s", cid, seq, time.Now(), pkt.retrans)
			if time.Now().After(pkt.retrans) {
				tlog.Printf("Textsocket retrans for channel:%d seq %d", cid, pkt.seq)
				ts.send(pkt)
				pkt.retransed++
				m := retransMin * time.Duration(1<<pkt.retransed)
				if pkt.retransed > 6 {
					m = retransMin * time.Duration(1<<6)
				}
				pkt.retrans = time.Now().Add(randretrans(m))
				if m < sleeptime {
					sleeptime = m
				}
			}
		}
		ts.lock.Unlock()
		//tlog.Printf("Textsocket retrans done")
		time.Sleep(sleeptime)
	}
}
func (ts *TextSocket) poll() {
	var err error
	for {
		tp := textPkt{}
		err = tp.ReadPkt(ts.io)
		if err != nil {
			break
		}
		if tp.id == ts.id {
			tlog.Printf("TextSocket pkt echo back ??")
			continue
		}
		if !tp.CheckCrc() {
			tlog.Printf("TextSocket pkt crc error")
			continue
		}
		func() {
			ts.lock.Lock()
			defer ts.lock.Unlock()
			c, ok := ts.channels[tp.channel]
			switch tp.t {
			case CONNECT:
				tlog.Printf("get connect msg for channel:%d", tp.channel)
				if ts.mode != 0 && ts.mode != SERVER {
					panic("client received connect")
				}
				if ok {
					// TODO retrans ??
					panic("already connected channel")
				}
				ts.cc <- tp.channel
			case CLOSE:
				tlog.Printf("get close msg for channel:%d", tp.channel)
				if ok {
					c.ec <- io.EOF
				}
			case ACK:
				tlog.Printf("channel:%d got ack pkt seq %d", tp.channel, tp.seq)
				if ok {
					c.wq.delete(tp.seq)
				}
			default:
				if tp.channel == 0 || !ok {
					return
				}
				tlog.Printf("channel:%d get data pkt, seq %d len %d", tp.channel, tp.seq, len(tp.data))
				if tp.seq < c.rseq {
					tlog.Printf("channel:%d got acked pkt seq %d rseq %d", tp.channel, tp.seq, c.rq.min)
					return
				}
				err := c.rq.add(&tp)
				if err != nil {
					tlog.Printf("channel:receive pkt failed for %s", err.Error())
					return
				}
				tlog.Printf("channel:%d ack pkt seq %d ", tp.channel, tp.seq)
				atp := &textPkt{
					t:       ACK,
					channel: c.cid,
					id:      ts.id,
					seq:     tp.seq,
					crc:     0,
					data:    []byte{},
				}
				atp.FillCrc()
				ts.send(atp)
				c.recv()
			}
		}()
	}
	ts.lock.Lock()
	defer ts.lock.Unlock()
	for _, c := range ts.channels {
		c.ec <- err
	}
}

const (
	PING = iota
	PONG
	CONNECT
	ACK
	DATA
	CLOSE
)
const (
	NONE = iota
	CLIENT
	SERVER
)

type textPkt struct {
	t       uint16
	channel uint16
	id      uint32
	seq     uint64
	crc     uint32
	data    []byte

	retrans   time.Time
	retransed int
}

func (tp *textPkt) FillCrc() {
	tp.crc = 0
	tp.crc = crc32.ChecksumIEEE(tp.Bytes())
}
func (tp *textPkt) CheckCrc() bool {
	c := tp.crc
	tp.crc = 0
	return c == crc32.ChecksumIEEE(tp.Bytes())
}
func (tp *textPkt) Bytes() []byte {
	buf := make([]byte, 24+len(tp.data))
	binary.LittleEndian.PutUint16(buf[:], tp.t)
	binary.LittleEndian.PutUint16(buf[2:], tp.channel)
	binary.LittleEndian.PutUint32(buf[4:], tp.id)
	binary.LittleEndian.PutUint64(buf[8:], tp.seq)
	binary.LittleEndian.PutUint32(buf[16:], tp.crc)
	binary.LittleEndian.PutUint32(buf[20:], uint32(len(tp.data)))
	copy(buf[24:], tp.data)
	return buf
}

func (tp *textPkt) ReadPkt(r io.Reader) error {
	buf := make([]byte, 24)
	err := readDone(r, buf)
	if err != nil {
		return err
	}
	tp.t = binary.LittleEndian.Uint16(buf[0:])
	tp.channel = binary.LittleEndian.Uint16(buf[2:])
	tp.id = binary.LittleEndian.Uint32(buf[4:])
	tp.seq = binary.LittleEndian.Uint64(buf[8:])
	tp.crc = binary.LittleEndian.Uint32(buf[16:])
	len := binary.LittleEndian.Uint32(buf[20:])
	tp.data = make([]byte, len)
	if len <= 0 {
		return nil
	}
	return readDone(r, tp.data)
}

type pktq struct {
	list []*textPkt
	size int
	min  uint64
	next uint64
}

func newpktq(size int) *pktq {
	return &pktq{
		list: make([]*textPkt, size),
		size: size,
		min:  0,
		next: 0,
	}
}
func (pq *pktq) first() *textPkt {
	idx := pq.min % uint64(pq.size)
	return pq.list[idx]
}
func (pq *pktq) full() bool {
	return pq.next-pq.min >= uint64(pq.size)
}
func (pq *pktq) add(pkt *textPkt) error {
	idx := pkt.seq % uint64(pq.size)
	if pkt.seq-pq.min >= uint64(pq.size) || pq.list[idx] != nil {
		return errors.New("pkt queue full")
	}
	pq.list[idx] = pkt
	if pkt.seq >= pq.next {
		pq.next = pkt.seq + 1
	}
	return nil
}
func (pq *pktq) delete(seq uint64) error {
	idx := seq % uint64(pq.size)
	if seq > pq.next || seq < pq.min || pq.list[idx] == nil {
		return errors.New(fmt.Sprintf("invalid seq%d, min%d,max%d", seq, pq.min, pq.next))
	}
	pq.list[idx] = nil
	for pq.min < pq.next {
		idx = pq.min % uint64(pq.size)
		if pq.list[idx] != nil {
			break
		}
		pq.min++
	}
	return nil
}
