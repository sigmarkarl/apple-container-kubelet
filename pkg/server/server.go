package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/containerd/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	remotecommandconsts "k8s.io/apimachinery/pkg/util/remotecommand"

	"github.com/containerd-shim-applevm-v2/pkg/hypervisor"
	"github.com/containerd-shim-applevm-v2/pkg/provider"
)

// Server serves the kubelet-compatible HTTPS API that the K8s API server
// proxies log requests to.
// PreviousLogProvider is the subset of the provider needed for serving previous logs.
type PreviousLogProvider interface {
	PreviousLogs(cID string) []byte
}

// Server serves the kubelet-compatible HTTPS API that the K8s API server
// proxies log requests to.
type Server struct {
	hv           hypervisor.Hypervisor
	srv          *http.Server
	port         int
	nodeIP       string
	prevLogs     PreviousLogProvider
	clientCAPath string
}

// New creates a new kubelet-compatible HTTPS server on the given port.
// If clientCAPath is non-empty, the server enforces mTLS by requiring clients
// to present a certificate signed by the given CA.
func New(hv hypervisor.Hypervisor, port int, nodeIP string, prevLogs PreviousLogProvider, clientCAPath string) *Server {
	retVal := &Server{
		hv:           hv,
		port:         port,
		nodeIP:       nodeIP,
		prevLogs:     prevLogs,
		clientCAPath: clientCAPath,
	}
	return retVal
}

// Run starts the HTTPS server and blocks until the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	tlsCfg, err := selfSignedTLSConfig(s.nodeIP, s.clientCAPath)
	if err != nil {
		return fmt.Errorf("generating TLS config: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /containerLogs/{namespace}/{pod}/{container}", s.handleContainerLogs)
	mux.HandleFunc("POST /exec/{namespace}/{pod}/{container}", s.handleExec)

	s.srv = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.port),
		Handler:      mux,
		TLSConfig:    tlsCfg,
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}

	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.srv.Addr, err)
	}
	tlsLn := tls.NewListener(ln, tlsCfg)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
	}()

	if s.clientCAPath != "" {
		log.G(ctx).WithField("clientCA", s.clientCAPath).Info("mTLS enabled: requiring client certificates")
	} else {
		log.G(ctx).Warn("mTLS disabled: kubelet server accepts unauthenticated connections")
	}
	log.G(ctx).WithField("port", s.port).Info("Kubelet HTTPS server listening")
	if err := s.srv.Serve(tlsLn); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}

func (s *Server) handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	pod := r.PathValue("pod")
	container := r.PathValue("container")

	follow, _ := strconv.ParseBool(r.URL.Query().Get("follow"))
	previous, _ := strconv.ParseBool(r.URL.Query().Get("previous"))

	cID := provider.ContainerIDFromNames(namespace, pod, container)

	if previous {
		data := s.prevLogs.PreviousLogs(cID)
		if len(data) == 0 {
			http.Error(w, "no previous logs available", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(data)
		return
	}

	rc, err := s.hv.Logs(r.Context(), cID, follow)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if follow {
		w.Header().Set("Transfer-Encoding", "chunked")
	}

	if f, ok := w.(http.Flusher); ok && follow {
		buf := make([]byte, 4096)
		for {
			n, readErr := rc.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				f.Flush()
			}
			if readErr != nil {
				break
			}
		}
	} else {
		_, _ = io.Copy(w, rc)
	}
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	pod := r.PathValue("pod")
	container := r.PathValue("container")
	q := r.URL.Query()

	supportedProtocols := []string{
		remotecommandconsts.StreamProtocolV4Name,
		remotecommandconsts.StreamProtocolV3Name,
		remotecommandconsts.StreamProtocolV2Name,
		remotecommandconsts.StreamProtocolV1Name,
	}
	negotiatedProtocol, err := httpstream.Handshake(r, w, supportedProtocols)
	if err != nil {
		log.G(r.Context()).WithError(err).Error("SPDY handshake failed")
		return
	}
	log.G(r.Context()).WithField("protocol", negotiatedProtocol).Debug("exec: SPDY handshake done")

	commands := q["command"]
	wantStdin, _ := strconv.ParseBool(q.Get("stdin"))
	wantStdout, _ := strconv.ParseBool(q.Get("stdout"))
	wantStderr, _ := strconv.ParseBool(q.Get("stderr"))
	wantOutput, _ := strconv.ParseBool(q.Get("output"))
	wantInput, _ := strconv.ParseBool(q.Get("input"))
	wantTTY, _ := strconv.ParseBool(q.Get("tty"))

	// "output"/"input" are alternative param names used by some kubectl versions.
	if wantOutput {
		wantStdout = true
	}
	if wantInput {
		wantStdin = true
	}

	expectedStreams := 1 // error stream is always present
	if wantStdin {
		expectedStreams++
	}
	if wantStdout {
		expectedStreams++
	}
	if wantTTY {
		// TTY mode: stdout and stderr are merged; kubectl sends a resize
		// stream instead of a separate stderr stream.
		expectedStreams++ // resize stream
	} else if wantStderr {
		expectedStreams++
	}

	var (
		stdinStream  io.Reader
		stdoutStream io.WriteCloser
		stderrStream io.WriteCloser
		errorStream  io.WriteCloser
		streamsMu    sync.Mutex
		arrived      int
		streamsCh    = make(chan struct{})
	)

	streamHandler := func(stream httpstream.Stream, replySent <-chan struct{}) error {
		streamType := stream.Headers().Get("streamType")
		log.G(r.Context()).WithField("streamType", streamType).Debug("exec: stream arrived")

		streamsMu.Lock()
		defer streamsMu.Unlock()

		switch streamType {
		case "stdin":
			stdinStream = stream
		case "stdout":
			stdoutStream = stream
		case "stderr":
			stderrStream = stream
		case "error":
			errorStream = stream
		case "resize":
			// Accepted but not used — we don't support terminal resizing.
		default:
			return fmt.Errorf("unexpected stream type: %s", streamType)
		}

		arrived++
		if arrived == expectedStreams {
			close(streamsCh)
		}
		return nil
	}

	upgrader := spdy.NewResponseUpgrader()
	conn := upgrader.UpgradeResponse(w, r, streamHandler)
	if conn == nil {
		// UpgradeResponse already wrote an error response.
		return
	}
	defer conn.Close()

	log.G(r.Context()).WithField("expectedStreams", expectedStreams).Debug("exec: waiting for streams")

	select {
	case <-streamsCh:
	case <-time.After(remotecommandconsts.DefaultStreamCreationTimeout):
		streamsMu.Lock()
		log.G(r.Context()).WithField("arrived", arrived).WithField("expected", expectedStreams).Error("timed out waiting for exec streams")
		streamsMu.Unlock()
		return
	}

	cID := provider.ContainerIDFromNames(namespace, pod, container)

	log.G(r.Context()).WithField("container", cID).WithField("commands", commands).Debug("exec: starting")

	execErr := s.hv.ExecInteractive(r.Context(), cID, commands, stdinStream, stdoutStream, stderrStream, wantTTY)

	if stdoutStream != nil {
		stdoutStream.Close()
	}
	if stderrStream != nil {
		stderrStream.Close()
	}

	writeExecStatus(errorStream, execErr)
	errorStream.Close()
}

func writeExecStatus(w io.Writer, execErr error) {
	status := &metav1.Status{Status: metav1.StatusSuccess}

	if execErr != nil {
		exitCode := 1
		if exitErr, ok := execErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		status = &metav1.Status{
			Status:  metav1.StatusFailure,
			Message: execErr.Error(),
			Reason:  remotecommandconsts.NonZeroExitCodeReason,
			Details: &metav1.StatusDetails{
				Causes: []metav1.StatusCause{
					{
						Type:    remotecommandconsts.ExitCodeCauseType,
						Message: strconv.Itoa(exitCode),
					},
				},
			},
		}
	}

	_ = json.NewEncoder(w).Encode(status)
}

// selfSignedTLSConfig builds a TLS configuration with a self-signed server
// certificate. When clientCAPath is non-empty, mTLS is enforced: only clients
// presenting a certificate signed by the given CA are accepted.
func selfSignedTLSConfig(nodeIP string, clientCAPath string) (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generating serial: %w", err)
	}

	sanIPs := []net.IP{net.ParseIP("127.0.0.1")}
	if nodeIP != "" {
		sanIPs = append(sanIPs, net.ParseIP(nodeIP))
	}

	tmpl := x509.Certificate{
		SerialNumber:          serialNumber,
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           sanIPs,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("creating certificate: %w", err)
	}

	retVal := &tls.Config{
		Certificates: []tls.Certificate{
			{
				Certificate: [][]byte{certDER},
				PrivateKey:  key,
			},
		},
		MinVersion: tls.VersionTLS12,
	}

	if clientCAPath != "" {
		caPool, loadErr := loadCertPool(clientCAPath)
		if loadErr != nil {
			return nil, fmt.Errorf("loading client CA: %w", loadErr)
		}
		retVal.ClientCAs = caPool
		retVal.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return retVal, nil
}

// loadCertPool reads a PEM file containing one or more certificates and returns
// an x509.CertPool suitable for use as a client CA pool.
func loadCertPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	pool := x509.NewCertPool()
	var found bool
	for len(data) > 0 {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, parseErr := x509.ParseCertificate(block.Bytes)
		if parseErr != nil {
			return nil, fmt.Errorf("parsing certificate in %s: %w", path, parseErr)
		}
		pool.AddCert(cert)
		found = true
	}

	if !found {
		return nil, fmt.Errorf("no certificates found in %s", path)
	}
	return pool, nil
}
