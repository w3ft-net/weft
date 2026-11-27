package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// selfSigned writes a self-signed certificate for localhost and returns the
// cert and key paths (the cert is its own CA bundle).
func selfSigned(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	kder, _ := x509.MarshalECPrivateKey(key)
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	return
}

func apply(t *testing.T, c Config) {
	t.Helper()
	if err := c.Apply(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { (&Config{}).Apply() })
}

// serve runs a health server with the process's server options and returns its address.
func serve(t *testing.T) string {
	t.Helper()
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := grpc.NewServer(ServerOptions()...)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func check(addr string) error {
	conn, err := grpc.NewClient(addr, DialOption())
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	return err
}

func TestPlaintextByDefault(t *testing.T) {
	apply(t, Config{})
	if err := check(serve(t)); err != nil {
		t.Fatalf("plaintext link: %v", err)
	}
}

func TestTLSWhenConfigured(t *testing.T) {
	cert, key := selfSigned(t)
	apply(t, Config{CertFile: cert, KeyFile: key, CAFile: cert})
	if err := check(serve(t)); err != nil {
		t.Fatalf("TLS link: %v", err)
	}
}

func TestAPlaintextClientCannotReachATLSServer(t *testing.T) {
	cert, key := selfSigned(t)
	apply(t, Config{CertFile: cert, KeyFile: key})
	addr := serve(t) // serves TLS; DialOption is plaintext (no CA)
	if err := check(addr); err == nil {
		t.Fatal("a plaintext client reached a TLS server")
	}
}

func TestApplyRefusesHalfAPair(t *testing.T) {
	cert, _ := selfSigned(t)
	if err := (&Config{CertFile: cert}).Apply(); err == nil {
		t.Fatal("a cert without a key was accepted")
	}
	if err := (&Config{CAFile: "/nonexistent"}).Apply(); err == nil {
		t.Fatal("an unreadable CA was accepted")
	}
}

func TestFlagsDefaultFromEnv(t *testing.T) {
	t.Setenv("WEFT_TLS_CA", "/etc/ca.pem")
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	c := DefineFlags(fs)
	fs.Parse(nil)
	if c.CAFile != "/etc/ca.pem" {
		t.Fatalf("CAFile = %q", c.CAFile)
	}
}
