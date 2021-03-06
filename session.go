package bidirpc

import (
	"bytes"
	"fmt"
	"io"
	"net/rpc"
	"sync"
)

var (
	streamTypeYin  byte = 1
	streamTypeYang byte = 2
)

const (
	defaultBufferPoolSize = 16
)

// Session is a bi-direction RPC connection.
type Session struct {
	conn      io.ReadWriteCloser
	yinOrYang bool
	writeLock sync.Mutex
	bp        *bufferPool

	streamYin  *stream
	streamYang *stream

	client *rpc.Client
	server *rpc.Server

	closeLock sync.Mutex
	closed    bool
	closedC   chan struct{}
}

// NewSession creates a new session.
func NewSession(conn io.ReadWriteCloser, yinOrYang bool, bufferPoolSize int) (*Session, error) {
	if bufferPoolSize == 0 {
		bufferPoolSize = defaultBufferPoolSize
	}
	s := &Session{
		conn:      conn,
		yinOrYang: yinOrYang,
		bp:        newBufferPool(bufferPoolSize),
		closedC:   make(chan struct{}),
	}

	s.streamYin = newStream(s, streamTypeYin)
	s.streamYang = newStream(s, streamTypeYang)

	var cliCodec *clientCodec
	var svrCodec *serverCodec
	if yinOrYang {
		cliCodec = newClientCodec(s.streamYin)
		svrCodec = newServerCodec(s.streamYang)
	} else {
		cliCodec = newClientCodec(s.streamYang)
		svrCodec = newServerCodec(s.streamYin)
	}
	s.client = rpc.NewClientWithCodec(cliCodec)
	s.server = rpc.NewServer()

	go s.server.ServeCodec(svrCodec)
	go s.readLoop()

	return s, nil
}

// Register publishes in the server the set of methods of the
// receiver value that satisfy the following conditions:
//  - exported method of exported type
//  - two arguments, both of exported type
//  - the second argument is a pointer
//  - one return value, of type error
// It returns an error if the receiver is not an exported type or has
// no suitable methods. It also logs the error using package log.
// The client accesses each method using a string of the form "Type.Method",
// where Type is the receiver's concrete type.
func (s *Session) Register(rcvr interface{}) error {
	return s.server.Register(rcvr)
}

// RegisterName is like Register but uses the provided name for the type
// instead of the receiver's concrete type.
func (s *Session) RegisterName(name string, rcvr interface{}) error {
	return s.server.RegisterName(name, rcvr)
}

// Go invokes the function asynchronously. It returns the Call structure representing
// the invocation. The done channel will signal when the call is complete by returning
// the same Call object. If done is nil, Go will allocate a new channel.
// If non-nil, done must be buffered or Go will deliberately crash.
func (s *Session) Go(serviceMethod string, args interface{}, reply interface{}, done chan *rpc.Call) *rpc.Call {
	return s.client.Go(serviceMethod, args, reply, done)
}

// Call invokes the named function, waits for it to complete, and returns its error status.
func (s *Session) Call(serviceMethod string, args interface{}, reply interface{}) error {
	return s.client.Call(serviceMethod, args, reply)
}

// Close closes the session.
func (s *Session) Close() error {
	s.doClose(nil)
	return nil
}

func (s *Session) readLoop() {
	var err error
	var header [4]byte
	var streamType byte
	var bodyLen int
	reader := io.LimitedReader{R: s.conn}

loop:
	for {
		_, err = io.ReadFull(s.conn, header[:])
		if err != nil {
			s.doClose(fmt.Errorf("read header error: %v", err))
			break loop
		}

		streamType, bodyLen = decodeHeader(header[:])
		if (streamType != streamTypeYin && streamType != streamTypeYang) || (bodyLen <= 0) {
			s.doClose(fmt.Errorf("read a invalid header"))
			break loop
		}

		body := s.bp.Get()
		body.Grow(bodyLen)
		reader.N = int64(bodyLen)
		_, err = io.Copy(body, &reader)
		if err != nil {
			s.bp.Put(body)
			s.doClose(fmt.Errorf("read body error: %v", err))
			break loop
		}

		var inC *chan *bytes.Buffer
		switch streamType {
		case streamTypeYin:
			inC = &s.streamYin.inC
		case streamTypeYang:
			inC = &s.streamYang.inC
		}
		select {
		case <-s.closedC:
			break loop
		case *inC <- body:
			// do nothing
		}
	}
}

func (s *Session) write(bytes []byte) error {
	s.writeLock.Lock()
	defer s.writeLock.Unlock()

	_, err := s.conn.Write(bytes)
	if err != nil {
		s.doClose(fmt.Errorf("write error: %v", err))
	}
	return err
}

func (s *Session) doClose(err error) {
	s.closeLock.Lock()
	defer s.closeLock.Unlock()

	if s.closed {
		return
	}
	s.closed = true

	//fmt.Printf("Session.doClose err=%v\n", err)
	close(s.closedC)
	s.conn.Close()
	s.client.Close()
}
