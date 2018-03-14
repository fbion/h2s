package h2s // import "ekyu.moe/h2s"

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Upstream is a HTTP proxy upstream that must support CONNECT method as defined
// in RFC7231 section 4.3.6.
type Upstream struct {
	Address  string `json:"address"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// Account is used for SOCKS5 authentication.
type Account struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Config is used to configure an h2s Server.
type Config struct {
	// HTTP proxy upstreams.
	Upstreams []*Upstream `json:"upstreams"`

	// With no Accounts, authentication is disabled.
	Accounts []*Account `json:"accounts,omitempty"`

	// Timeout value when dialing to a upstream. Default "20s".
	Timeout string `json:"timeout"`

	// The max retries count of dialing to upstreams. Default 3.
	Retries *int `json:"retries"`
}

type internalUpstream struct {
	address string
	header  http.Header
}

type internalAccount struct {
	username []byte
	password []byte
}

// Server is a SOCKS5 server that forward all incoming requests via Upstreams
// by HTTP/1.1 CONNECT.
type Server struct {
	next        chan *internalUpstream
	stop        chan struct{}
	requireAuth bool
	account     []*internalAccount
	retries     int
	dialer      *net.Dialer

	isClosed bool
	mu       sync.Mutex
}

// I know, I know
func basicauth(username, password string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", "")
	if username != "" && password != "" {
		combined := username + ":" + password
		encoded := base64.StdEncoding.EncodeToString([]byte(combined))
		h.Set("Proxy-Authorization", "Basic "+encoded)
	}

	return h
}

// NewServer creates an h2s server instance.
func NewServer(c *Config) (*Server, error) {
	s := &Server{}

	if c.Timeout != "" {
		timeout, err := time.ParseDuration(c.Timeout)
		if err != nil {
			return nil, errors.New("h2s: create server: " + err.Error())
		}
		s.dialer = &net.Dialer{Timeout: timeout}
	} else {
		s.dialer = &net.Dialer{Timeout: 20 * time.Second}
	}

	if c.Retries != nil {
		s.retries = *c.Retries
	} else {
		s.retries = 3
	}

	s.requireAuth = len(c.Accounts) > 0
	if s.requireAuth {
		s.account = make([]*internalAccount, len(c.Accounts))
		for i, v := range c.Accounts {
			s.account[i] = &internalAccount{
				username: []byte(v.Username),
				password: []byte(v.Password),
			}
		}
	}

	if len(c.Upstreams) == 0 {
		return nil, errors.New("h2s: create server: no upstreams")
	}

	upstreams := make([]*internalUpstream, len(c.Upstreams))
	for i, v := range c.Upstreams {
		addr := v.Address
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			addr += ":80"
			host, port, err = net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("h2s: create server: invalid address %q", v.Address)
			}
		}
		addr = net.JoinHostPort(host, port)

		upstreams[i] = &internalUpstream{
			address: addr,
			header:  basicauth(v.Username, v.Password),
		}
	}

	s.next = make(chan *internalUpstream)
	s.stop = make(chan struct{})
	go func() {
		for {
			for _, v := range upstreams {
				select {
				case s.next <- v:
				case <-s.stop:
					close(s.next)
					return
				}
			}
		}
	}()

	return s, nil
}

// Close closes s. Already established connections will not be affected.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.isClosed {
		return errors.New("h2s: server is already closed")
	}
	s.stop <- struct{}{}
	s.isClosed = true
	s.mu.Unlock()

	return nil
}

// Serve handles a net.Conn, reads request from it with SOCKS5 format, and dispatch
// the request via HTTP CONNECT. Serve closes conn whether an error occurs or
// connection is done. The caller must not use conn again.
func (s *Server) Serve(conn net.Conn) error {
	defer conn.Close()

	// this is bad
	s.mu.Lock()
	if s.isClosed {
		return errors.New("h2s: server is closed")
	}
	s.mu.Unlock()

	if err := s.handshake(conn); err != nil {
		return errors.New("h2s: handshake: " + err.Error())
	}

	target, err := s.readRequest(conn)
	if err != nil {
		return errors.New("h2s: read request: " + err.Error())
	}

	out, u, err := s.dialUpstream()
	if err != nil {
		return errors.New("h2s: dial upstream: " + err.Error())
	}
	defer out.Close()

	if err := s.handshakeUpstream(out, u, target); err != nil {
		return errors.New("h2s: handshake upstream: " + err.Error())
	}

	// sync
	duplexPipe(out, conn)

	return nil
}
