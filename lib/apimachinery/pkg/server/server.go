package server

import (
	"context"
	"crypto/tls"
	"github.com/projectcalico/calico/lib/apimachinery/pkg/handler"
	"net"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
)

// HTTPServer is the interface that most, if not all, our http servers need to implement. It allows for starting tls /
// non tls servers, and waiting for the server too shutdown.
type HTTPServer interface {
	ListenAndServeTLS(context.Context) error

	// TODO should we remove the ability to serve non TLS?
	ListenAndServe(context.Context) error
	WaitForShutdown() error
}

type httpServer struct {
	srv         *http.Server
	tlsConfig   *tls.Config
	addr        string
	shutdownCtx context.Context
	serverErrs  chan error
}

type Router interface {
	RegisterAPIs([]handler.API, ...handler.MiddlewareFunc) http.Handler
}

func NewHTTPServer(router Router, apis []handler.API, options ...Option) (HTTPServer, error) {
	const (
		defaultIdleTimeout  = 120 * time.Second
		defaultReadTimeout  = 5 * time.Second
		defaultWriteTimeout = 10 * time.Second
	)

	srv := &httpServer{
		srv: &http.Server{
			IdleTimeout:  defaultIdleTimeout,
			ReadTimeout:  defaultReadTimeout,
			WriteTimeout: defaultWriteTimeout,
		},
		serverErrs: make(chan error, 1),
	}

	for _, option := range options {
		if err := option(srv); err != nil {
			return nil, err
		}
	}

	srv.srv.Addr = srv.addr
	srv.srv.TLSConfig = srv.tlsConfig
	srv.srv.Handler = router.RegisterAPIs(apis)

	return srv, nil
}

func (s *httpServer) ListenAndServeTLS(ctx context.Context) error {
	s.shutdownCtx = ctx

	addr := s.srv.Addr
	if addr == "" {
		addr = ":https"
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	go func() {
		defer ln.Close()
		s.serverErrs <- s.srv.ServeTLS(ln, "", "")
		close(s.serverErrs)
	}()

	return nil
}

func (s *httpServer) ListenAndServe(ctx context.Context) error {
	addr := s.srv.Addr
	if addr == "" {
		addr = ":https"
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	s.shutdownCtx = ctx
	go func() {
		defer ln.Close()
		s.serverErrs <- s.srv.Serve(ln)
		close(s.serverErrs)
	}()

	return nil
}

func (s *httpServer) WaitForShutdown() error {
	var err error
	select {
	case <-s.shutdownCtx.Done():
		log.Info("Received shutdown signal, shutting server down.")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = s.srv.Shutdown(ctx)
	case err = <-s.serverErrs:
	}
	return err
}
