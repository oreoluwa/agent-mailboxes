//go:build ignore

// Command test-tls-proxy exposes two disposable implicit-TLS listeners that
// forward to plaintext test services. It is used only by local smoke tests.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"sync"
	"time"
)

func main() {
	imapTarget := flag.String("imap-target", "", "plaintext IMAP target")
	smtpTarget := flag.String("smtp-target", "", "plaintext SMTP target")
	caOut := flag.String("ca-out", "", "path for disposable CA certificate")
	flag.Parse()
	if *imapTarget == "" || *smtpTarget == "" || *caOut == "" {
		fail(fmt.Errorf("imap-target, smtp-target, and ca-out are required"))
	}
	certificate, certPEM, err := disposableCertificate()
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(*caOut, certPEM, 0o600); err != nil {
		fail(err)
	}
	tlsConfig := tlsConfig(certificate)
	imapListener, err := tlsListen(tlsConfig)
	if err != nil {
		fail(err)
	}
	defer imapListener.Close()
	smtpListener, err := tlsListen(tlsConfig)
	if err != nil {
		fail(err)
	}
	defer smtpListener.Close()
	_ = json.NewEncoder(os.Stdout).Encode(map[string]int{
		"imap_port": imapListener.Addr().(*net.TCPAddr).Port,
		"smtp_port": smtpListener.Addr().(*net.TCPAddr).Port,
	})
	var wait sync.WaitGroup
	wait.Add(2)
	go serve(imapListener, *imapTarget, &wait)
	go serve(smtpListener, *smtpTarget, &wait)
	wait.Wait()
}

func tlsConfig(certificate tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
}

func tlsListen(config *tls.Config) (net.Listener, error) {
	return tls.Listen("tcp", "0.0.0.0:0", config)
}

func disposableCertificate() (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "agent-emails-smoke"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost", "host.docker.internal"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	return certificate, certPEM, err
}

func serve(listener net.Listener, target string, wait *sync.WaitGroup) {
	defer wait.Done()
	for {
		incoming, err := listener.Accept()
		if err != nil {
			return
		}
		go proxy(incoming, target)
	}
}

func proxy(incoming net.Conn, target string) {
	defer incoming.Close()
	outgoing, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return
	}
	defer outgoing.Close()
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(outgoing, incoming)
		if tcp, ok := outgoing.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}()
	_, _ = io.Copy(incoming, outgoing)
	<-done
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
