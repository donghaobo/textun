package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
)

type base64RW struct {
	rw  io.ReadWriter
	br  *bufio.Reader
	buf []byte
}

func (b64 *base64RW) String() string {
	return fmt.Sprintf("base64rw")
}
func NewBase64Writer(rw io.ReadWriter) *base64RW {
	return &base64RW{
		rw:  rw,
		br:  bufio.NewReaderSize(rw, 1024*1024),
		buf: []byte{},
	}
}

func (bw *base64RW) Write(b []byte) (n int, err error) {
	bl := len(b)
	tlog.Printf("base64 trying to write %d bytes", bl)
	l := base64.RawStdEncoding.EncodedLen(bl)
	buf := make([]byte, l+1)
	base64.RawStdEncoding.Encode(buf, b)
	buf[l] = '\n'
	err = writeDone(bw.rw, buf)
	if err != nil {
		// should return real length ??
		return 0, err
	}
	tlog.Printf("base64 write %d bytes done ", bl)
	return bl, nil
}
func (bw *base64RW) Read(b []byte) (n int, err error) {
	tlog.Printf("base64 trying to read %d bytes", len(b))
	if len(bw.buf) != 0 {
		cn := copy(b, bw.buf)
		if cn != n {
			bw.buf = bw.buf[cn:]
		}
		tlog.Printf("base64 got %d bytes from buf, left %d bytes", cn, len(bw.buf))
		return cn, nil
	}
restart:
	line, prefix, err := bw.br.ReadLine()
	if prefix && err != nil {
		for prefix && err != nil {
			tlog.Printf("base64 got too lone lines %d", len(line))
			line, prefix, err = bw.br.ReadLine()
		}
		goto restart
	}
	if err != nil {
		return 0, err
	}
	if len(line) > 2000 {
		tlog.Printf("base64 got too lone lines %d", len(line))
		// we only encode 1000bytes once
		goto restart
	}

	dbl := base64.RawStdEncoding.DecodedLen(len(line))
	dbuf := make([]byte, dbl)
	_, err = base64.RawStdEncoding.Decode(dbuf, line)
	cn := copy(b, dbuf)
	if cn != n {
		bw.buf = dbuf[cn:]
	}
	tlog.Printf("base64 got %d bytes , left %d bytes", cn, len(bw.buf))
	return cn, nil
}
