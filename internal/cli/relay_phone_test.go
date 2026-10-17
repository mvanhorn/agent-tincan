package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// relayHelperTLSEnv makes the helper relay's tailnet offer HTTPS, as a
// tailnet with certificates turned on does.
const relayHelperTLSEnv = "TINCAN_TEST_RELAY_TLS"

// testCertName is the certificate name the helper relay's HTTPS listener
// serves, standing in for its MagicDNS name.
const testCertName = "tincan-relay.example.ts.net"

// loopbackTLSTailnet is loopbackTailnet with HTTPS: ListenTLS serves a
// self-signed certificate for testCertName on loopback and writes the
// listener's URL to tlsurl in the state dir.
type loopbackTLSTailnet struct {
	loopbackTailnet
	dir string
}

func (l loopbackTLSTailnet) ListenTLS(context.Context) (net.Listener, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: testCertName}, DNSNames: []string{testCertName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, "", err
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(filepath.Join(l.dir, "tlsurl"), []byte("https://"+ln.Addr().String()), 0o600); err != nil {
		ln.Close()
		return nil, "", err
	}
	return ln, testCertName, nil
}

// waitLog waits for the relay process to log want.
func (p *relayProcess) waitLog(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(p.out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("relay never logged %q:\n%s", want, p.out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func openPage(t *testing.T, c *http.Client, base, host string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "GET", base+"/phone/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// With no certificates from the tailnet, the relay still starts, says why
// there is no HTTPS, and serves the page on its plain listener.
func TestRelayProcessPhonePageWithoutHTTPS(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a relay process")
	}
	p := startRelayProcess(t)
	p.waitLog(t, "phone page: "+p.url+"/phone/")
	if !strings.Contains(p.out.String(), "no HTTPS") || !strings.Contains(p.out.String(), "push alerts are off") {
		t.Fatalf("relay did not say why the page has no HTTPS:\n%s", p.out)
	}
	if code := openPage(t, http.DefaultClient, p.url, ""); code != http.StatusOK {
		t.Fatalf("admin opening the page over HTTP: %d", code)
	}
}

// With certificates, the relay serves the page over HTTPS under its
// certificate name, behind the same browser hardening as its plain
// listener.
func TestRelayProcessPhonePageOverHTTPS(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a relay process")
	}
	t.Setenv(relayHelperTLSEnv, "1")
	p := startRelayProcess(t)
	p.waitLog(t, "phone page: https://"+testCertName+"/phone/")
	raw, err := os.ReadFile(filepath.Join(p.dir, "tlsurl"))
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if code := openPage(t, c, string(raw), testCertName); code != http.StatusOK {
		t.Fatalf("admin opening the page over HTTPS: %d", code)
	}
	if code := openPage(t, c, string(raw), "evil.example"); code != http.StatusForbidden {
		t.Fatalf("page over HTTPS under a rebound name: %d, want 403", code)
	}
}

// phoneTLS asks the tailnet for HTTPS and passes on why it has none.
func TestPhoneTLSFromTheTailnet(t *testing.T) {
	if _, _, err := phoneTLS(t.Context(), loopbackTailnet{}); err == nil {
		t.Fatal("a tailnet connection that cannot serve HTTPS gave a listener")
	}
	off := errors.New("HTTPS certificates are off")
	if _, _, err := phoneTLS(t.Context(), failingTLS{err: off}); !errors.Is(err, off) {
		t.Fatalf("phoneTLS = %v, want %v", err, off)
	}
	ln, name, err := phoneTLS(t.Context(), loopbackTLSTailnet{dir: t.TempDir()})
	if err != nil || name != testCertName {
		t.Fatalf("phoneTLS = %q %v", name, err)
	}
	ln.Close()
}

type failingTLS struct {
	loopbackTailnet
	err error
}

func (f failingTLS) ListenTLS(context.Context) (net.Listener, string, error) { return nil, "", f.err }
