package fv_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	url2 "net/url"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	. "github.com/onsi/gomega"
	"golang.org/x/net/http2"

	calicotls "github.com/projectcalico/calico/crypto/pkg/tls"
	"github.com/projectcalico/calico/guardian/pkg/config"
	"github.com/projectcalico/calico/guardian/pkg/daemon"
	"github.com/projectcalico/calico/guardian/pkg/server"
	"github.com/projectcalico/calico/lib/std/cryptoutils"
)

func init() {
	var err error
	http2Transport, err = http2.ConfigureTransports(http.DefaultTransport.(*http.Transport))
	if err != nil {
		panic(err)
	}
}

func tlsConfigProvider(tlsCert tls.Certificate, ca cryptoutils.CA) config.TLSConfigProviderFunc {
	return func() (*tls.Config, *tls.Certificate, error) {
		tlsConfig, err := calicotls.NewTLSConfig()
		Expect(err).ShouldNot(HaveOccurred())

		tlsConfig.Certificates = []tls.Certificate{tlsCert}
		tlsConfig.RootCAs = x509.NewCertPool()
		tlsConfig.ServerName = ca.Certificate().DNSNames[0]

		Expect(ca.AddToCertPool(tlsConfig.RootCAs)).ShouldNot(HaveOccurred())
		return tlsConfig, &tlsCert, nil
	}
}

var http2Transport *http2.Transport

func upstreamTLSConfig(tlsCert tls.Certificate, ca cryptoutils.CA) *tls.Config {
	tlsCfg, err := calicotls.NewTLSConfig()
	Expect(err).ShouldNot(HaveOccurred())

	tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	tlsCfg.ClientCAs = x509.NewCertPool()
	Expect(ca.AddToCertPool(tlsCfg.ClientCAs)).ShouldNot(HaveOccurred())

	tlsCfg.Certificates = []tls.Certificate{tlsCert}
	return tlsCfg
}

func TestRequestsFromUpstreamToDownstream(t *testing.T) {
	setup(t)

	testSrvAddr := "localhost:8999"
	startTestService(testSrvAddr)

	cfg := config.Config{
		LogLevel:                "DEBUG",
		TunnelDialRetryAttempts: -1,
		TunnelDialRetryInterval: 100 * time.Millisecond,
		TunnelDialTimeout:       5 * time.Second,
		VoltronURL:              "localhost:8443",
		KeepAliveEnable:         true,
		KeepAliveInterval:       1000000,
	}

	clientCA, clientCert := createTLSConfigTLSConfig()
	serverCA, serverCert := createTLSConfigTLSConfig()

	cfg.SetTLSConfigProvider(tlsConfigProvider(*clientCert, serverCA))
	tlsCfg := upstreamTLSConfig(*serverCert, clientCA)

	upstreamSrv := newUpstreamServer(":8443", tlsCfg)
	defer upstreamSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	daemonDoneSig := make(chan struct{})
	go func() {
		defer close(daemonDoneSig)

		url, err := url2.Parse("https://" + testSrvAddr)
		Expect(err).ShouldNot(HaveOccurred())
		daemon.Run(ctx, cfg, []server.Target{
			{Path: "/", Dest: url, AllowInsecureTLS: true},
		})
	}()

	req, err := http.NewRequest(http.MethodGet, "https://"+testSrvAddr+"/foobar", nil)
	Expect(err).ShouldNot(HaveOccurred())

	resp, err := upstreamSrv.SendRequest(ctx, req)
	Expect(err).ShouldNot(HaveOccurred())
	Expect(resp.StatusCode).Should(Equal(http.StatusOK))
	defer resp.Body.Close()

	var rspObj testServerResponse
	Expect(json.NewDecoder(resp.Body).Decode(&rspObj)).ShouldNot(HaveOccurred())
	Expect(rspObj.Message).Should(Equal("hello from the other side."))

	// Close the mux and allow guardian to try to reconnect, as we want to test that we can still use the tunnel after
	// reconnecting.
	t.Log("test that the guardian continuously tries to reconnect to the upstream server when the connection is closed")
	upstreamSrv.rejectConnections = true   // Reject all connection requests from guardian
	upstreamSrv.connectionRequestCount = 0 // Reset the request connect
	_ = upstreamSrv.mux.Close()

	// Wait for guardian to try connecting at least 5 times to ensure the reconnection logic retries frequently based on
	// the configuration. The interval is set to 100 ms, so it should take any longer than 5 seconds for 5 attempts.
	Eventually(func() int { return upstreamSrv.connectionRequestCount }, "5s").Should(BeNumerically(">=", 5))

	// Allow guardian to reconnect to the upstream server.
	upstreamSrv.rejectConnections = false

	// Send a request to the upstream server and ensure it works after reconnecting.
	req, err = http.NewRequest(http.MethodGet, "https://localhost:8999/foobar", nil)
	Expect(err).ShouldNot(HaveOccurred())

	resp, err = upstreamSrv.SendRequest(ctx, req)
	Expect(err).ShouldNot(HaveOccurred())
	Expect(resp.StatusCode).Should(Equal(http.StatusOK))

	defer resp.Body.Close()

	rspObj = testServerResponse{}
	Expect(json.NewDecoder(resp.Body).Decode(&rspObj)).ShouldNot(HaveOccurred())
	Expect(rspObj.Message).Should(Equal("hello from the other side."))

	cancel()
	<-daemonDoneSig
}

func TestRequestsFromDownstreamToUpstream(t *testing.T) {
	setup(t)

	cfg := config.Config{
		LogLevel:                "DEBUG",
		TunnelDialRetryAttempts: -1,
		TunnelDialRetryInterval: 100 * time.Millisecond,
		TunnelDialTimeout:       500 * time.Second,
		VoltronURL:              "localhost:8443",
		KeepAliveEnable:         true,
		KeepAliveInterval:       1000000,
		Listen:                  true,
		ListenPort:              "9090",
	}

	clientCA, clientCert := createTLSConfigTLSConfig()
	serverCA, serverCert := createTLSConfigTLSConfig()

	cfg.SetTLSConfigProvider(tlsConfigProvider(*clientCert, serverCA))
	tlsCfg := upstreamTLSConfig(*serverCert, clientCA)

	upstreamSrv := newUpstreamServer(":8443", tlsCfg)
	defer upstreamSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	daemonDoneSig := make(chan struct{})
	go func() {
		defer close(daemonDoneSig)

		daemon.Run(ctx, cfg, []server.Target{})
	}()

	srv := http.Server{
		Addr: "localhost:9090",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(testServerResponse{Message: "hello from the other side."})
		}),
	}
	go func() {
		upstreamSrv.waitForReady(ctx)

		if err := srv.Serve(upstreamSrv.mux); err != nil {
			return
		}
	}()

	upstreamSrv.waitForReady(ctx)

	cli := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}

	req, err := http.NewRequest(http.MethodGet, "http://localhost:9090/foobar", nil)
	Expect(err).ShouldNot(HaveOccurred())

	rspObj := sendRequest(cli, req)
	Expect(rspObj.Message).Should(Equal("hello from the other side."))

	cancel()
	<-daemonDoneSig
}

func sendRequest(cli *http.Client, req *http.Request) testServerResponse {
	resp, err := cli.Do(req)
	Expect(err).ShouldNot(HaveOccurred())
	defer resp.Body.Close()

	rspObj := testServerResponse{}
	Expect(json.NewDecoder(resp.Body).Decode(&rspObj)).ShouldNot(HaveOccurred())
	return rspObj
}

type testServerResponse struct {
	Message string `json:"message"`
}

func startTestService(addr string) {
	tmpDir := os.TempDir()
	fooServerCert, fooServerKey := createKeyCertPair(tmpDir, "foo-server.crt", "foo-server.key")
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/foobar", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(testServerResponse{Message: "hello from the other side."})
		})
		srv := http.Server{
			Addr:    addr,
			Handler: mux,
		}
		if err := srv.ListenAndServeTLS(fooServerCert, fooServerKey); err != nil {
			panic(err)
		}
	}()
}

type upstreamServer struct {
	close                  chan struct{}
	mux                    *yamux.Session
	connectionRequestCount int
	rejectConnections      bool
}

func newUpstreamServer(addr string, tlsCfg *tls.Config) *upstreamServer {
	listener, err := net.Listen("tcp", addr)
	Expect(err).ShouldNot(HaveOccurred())
	listener = tls.NewListener(listener, tlsCfg)

	srv := &upstreamServer{
		close: make(chan struct{}),
	}

	// Use a different go routine to accept connections so we don't block getting mux connections while waiting to accept.
	connCh := make(chan net.Conn)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			srv.connectionRequestCount++
			if srv.rejectConnections {
				_ = conn.Close()
			} else {
				connCh <- conn
			}
		}
	}()

	go func() {
		defer func() {
			if srv.mux != nil {
				_ = srv.mux.Close()
			}
		}()
		for {
			select {
			case <-srv.close:
				_ = listener.Close()
				return
			case conn := <-connCh:
				if srv.mux != nil && !srv.mux.IsClosed() {
					continue
				}
				cfg := yamux.DefaultConfig()
				cfg.ConnectionWriteTimeout = 5 * time.Minute
				cfg.AcceptBacklog = 1000
				cfg.EnableKeepAlive = true
				cfg.KeepAliveInterval = 10000

				if tlsc, ok := conn.(*tls.Conn); ok {
					if !tlsc.ConnectionState().HandshakeComplete {
						if err := tlsc.Handshake(); err != nil {
							panic(err)
						}
						certs := tlsc.ConnectionState().PeerCertificates
						_ = certs
					}

				}

				srv.mux, err = yamux.Server(conn, cfg)

				// We never expect an error here, so something is wrong with the test if we get an error.
				if err != nil {
					panic(err)
				}
			}
		}
	}()

	return srv
}

func (srv *upstreamServer) waitForReady(ctx context.Context) {
	for srv.mux == nil || srv.mux.IsClosed() {
		time.Sleep(500 * time.Millisecond)
		if ctx.Err() != nil {
			panic(ctx.Err())
		}
	}
}

func (srv *upstreamServer) SendRequest(ctx context.Context, req *http.Request) (*http.Response, error) {
	srv.waitForReady(ctx)

	conn, err := srv.mux.Open()
	Expect(err).ShouldNot(HaveOccurred())

	tlsConn := tls.Client(conn, &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2"},
	})

	// We never expect an error here, so something is wrong with the test if we get an error.
	if err := tlsConn.Handshake(); err != nil {
		return nil, err
	}

	http2Conn, err := http2Transport.NewClientConn(tlsConn)
	Expect(err).ShouldNot(HaveOccurred())

	return http2Conn.RoundTrip(req)
}

func (srv *upstreamServer) Close() {
	close(srv.close)
}

func createTLSConfigTLSConfig() (cryptoutils.CA, *tls.Certificate) {
	ca, err := cryptoutils.NewCA("ca-signer", cryptoutils.WithDNSNames("localhost"))
	Expect(err).ShouldNot(HaveOccurred())

	certificate, err := ca.CreateTLSCertificate("test-managed-cluster",
		cryptoutils.WithDNSNames("localhost"),
		cryptoutils.WithKeyUsages(x509.KeyUsageKeyEncipherment, x509.KeyUsageDigitalSignature),
		cryptoutils.WithExtKeyUsages(x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth),
	)
	Expect(err).ShouldNot(HaveOccurred())

	return ca, certificate
}
