package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"textun/pty"
	"unicode"

	"github.com/google/goterm/term"
)

var (
	mode         = flag.String("m", "client", "client or server")
	prefix       = flag.String("p", "axxx====", "prefix")
	listen_addr  = flag.String("l", "[::]:8888", "client listen addr")
	connect_addr = flag.String("c", "127.0.0.1:80", "server connect addr")
	runCmd       = flag.String("r", os.Getenv("SHELL"), "run cmd, shell default")
	logFile      = flag.String("f", "/dev/null", "logfile textun.log default ")
	tlog         *log.Logger
	stdlc        *LineChan
)

func getWords(line string) []string {
	quot := false
	backslash := false
	comment := false
	words := strings.FieldsFunc(line, func(r rune) bool {
		if comment {
			return true
		}
		if backslash {
			backslash = false
			return false
		}
		switch r {
		case '\\':
			backslash = true
			return false
		case '"':
			quot = !quot
			return true
		default:
			if quot {
				return false
			} else if r == '#' {
				comment = true
				return true
			} else {
				return unicode.IsSpace(r)
			}
		}
	})
	for i, w := range words {
		w = strings.ReplaceAll(w, `\ `, ` `)
		w = strings.ReplaceAll(w, `\"`, `"`)
		w = strings.ReplaceAll(w, `\#`, `#`)
		w = strings.ReplaceAll(w, `\\`, `\`)
		words[i] = w
	}
	return words
}

func main() {
	fmt.Printf("args is %#v\n", os.Args)
	flag.Parse()
	file, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		panic("Failed to open error log file:" + *logFile)
	}
	tlog = log.New(file, "", log.Lshortfile|log.LstdFlags)
	if *mode == "client" {
		runClient()
	} else if *mode == "server" {
		server()
	} else {
		flag.Usage()
	}
}

func relay1(in io.ReadWriteCloser, out io.ReadWriteCloser, bufLen int) {
	buf := make([]byte, bufLen)
	instr := fmt.Sprint(in)
	if inn, ok := in.(net.Conn); ok {
		instr = inn.RemoteAddr().String()
	}
	outstr := fmt.Sprint(out)
	if outn, ok := out.(net.Conn); ok {
		outstr = outn.RemoteAddr().String()
	}
	for {
		tlog.Printf("relay trying to read %d bytes from %s", bufLen, instr)
		n, err := in.Read(buf)
		if err != nil {
			tlog.Printf("read failed for %s", err.Error())
			out.Close()
			return
		}
		tlog.Printf("relay got %d bytes from %s", n, instr)
		err = writeDone(out, buf[:n])
		if err != nil {
			tlog.Printf("write failed for %s", err.Error())
			in.Close()
			return
		}
		tlog.Printf("relay writed %d bytes to %s", n, outstr)
	}
}

func relay(a, b io.ReadWriteCloser, bufLen int) {
	go relay1(a, b, bufLen)
	go relay1(b, a, bufLen)
}

type cio struct {
	in  io.Reader
	out io.Writer
}

func (c *cio) String() string {
	return fmt.Sprintf("cio:in:%+v:out%+v", c.in, c.out)
}
func (io *cio) Read(b []byte) (n int, err error) {
	return io.in.Read(b)
}
func (io *cio) Write(b []byte) (n int, err error) {
	return io.out.Write(b)
}
func (io *cio) Close() error {
	return nil
}
func runClient() {
	m, ss, err := pty.Open()
	if err != nil {
		panic("err pty")
	}
	defer m.Close()
	s, err := os.OpenFile(ss, os.O_RDWR, 0)
	if err != nil {
		panic("err pty")
	}
	defer s.Close()
	// Save the current Stdin attributes
	backupTerm, _ := term.Attr(os.Stdin)
	// Copy attributes
	myTerm := backupTerm
	// Change the Stdin term to RAW so we get everything
	myTerm.Raw()
	myTerm.Set(os.Stdin)
	// Set the backup attributes on our PTY slave
	backupTerm.Set(s)
	// Make sure we'll get the attributes back when exiting
	defer backupTerm.Set(os.Stdin)
	// Start up the slaveshell
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGWINCH, syscall.SIGCHLD)
	cmds := getWords(*runCmd)
	fmt.Printf("cmd is %#v\n", cmds)
	cmd := exec.Command(cmds[0], cmds[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s, s, s
	//cmd.Args = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true}
	err = cmd.Start()
	if err != nil {
		panic("err exec cmd")
	}
	// Get the initial winsize
	myTerm.Winsz(os.Stdin)
	myTerm.Winsz(s)

	stdio := &cio{os.Stdin, os.Stdout}
	lm := NewLineMux(m)
	stdlc, err = lm.NewChannel("")
	if err != nil {
		panic("could not create channel for line mux")
	}
	go relay(stdio, stdlc, 100000)
	lc, err := lm.NewChannel(*prefix)
	if err != nil {
		panic("could not create channel for line mux")
	}
	ts := NewTextSocket(NewBase64Writer(lc))
	go clientListen(ts)
	for {
		switch <-sig {
		case syscall.SIGWINCH:
			myTerm.Winsz(os.Stdin)
			myTerm.Setwinsz(s)
		default:
			return
		}
	}
}

func clientListen(ts *TextSocket) {
	tlog.Printf("client started")
	l, err := net.Listen("tcp", *listen_addr)
	if err != nil {
		panic(fmt.Sprintf("listen to %s failed", *listen_addr))
	}
	tlog.Printf("client listen done")
	for {
		conn, err := l.Accept()
		if err != nil {
			panic("accept failed")
		}
		tlog.Printf("new connection")
		tc, err := ts.Connect()
		if err != nil {
			panic("connect failed")
		}
		tlog.Printf("connected to text socket")
		go relay(conn, tc, 1000)
	}
}

func server() {
	fmt.Printf("server started\n")
	stdio := &cio{os.Stdin, os.Stdout}
	lm := NewLineMux(stdio)
	lc, err := lm.NewChannel(*prefix)
	if err != nil {
		panic("could not create channel for line mux")
	}
	ts := NewTextSocket(NewBase64Writer(lc))
	tlog.Printf("server started")
	for {
		tc, err := ts.Accept()
		if err != nil {
			panic("accept failed")
		}
		conn, err := net.Dial("tcp", *connect_addr)
		if err != nil {
			panic(fmt.Sprintf("connect to %s failed", *connect_addr))
		}
		go relay(conn, tc, 1000)
	}
}
