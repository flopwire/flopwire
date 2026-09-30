package main

// Server transport (D13). The server always speaks TLS:
//
//   - self-signed (the default): a certificate generated on first start and
//     kept in the TLS directory. Devices pin its SHA-256 fingerprint, which
//     `flopwire serve` logs, `flopwire fingerprint` prints and invites carry.
//   - acme: a public domain (--domain) gets a certificate from Let's Encrypt
//     through TLS-ALPN-01, so the listener must be reachable on port 443.
//   - proxy: plain HTTP for an operator's own TLS-terminating reverse proxy.
//   - off: plain HTTP on a loopback address only.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"golang.org/x/crypto/acme/autocert"
)

const (
	tlsSelfSigned = "self-signed"
	tlsACME       = "acme"
	tlsProxy      = "proxy"
	tlsOff        = "off"
)

// tlsFlags are serve's transport flags.
type tlsFlags struct {
	mode, dir, domains, email string
}

func addTLSFlags(fs *flag.FlagSet) *tlsFlags {
	f := &tlsFlags{}
	fs.StringVar(&f.mode, "tls", env("FLOPWIRE_TLS", "auto"), "transport: auto (acme with --domain, else self-signed), self-signed, acme, proxy (plain HTTP behind your TLS proxy) or off (plain HTTP, loopback only)")
	fs.StringVar(&f.dir, "tls-dir", env("FLOPWIRE_TLS_DIR", defaultTLSDir()), "directory for the self-signed certificate and the ACME cache")
	fs.StringVar(&f.domains, "domain", env("FLOPWIRE_DOMAIN", ""), "public domain name(s) for an ACME certificate, comma-separated")
	fs.StringVar(&f.email, "acme-email", env("FLOPWIRE_ACME_EMAIL", ""), "contact email for the ACME account (optional)")
	return f
}

func defaultTLSDir() string {
	if base, err := os.UserConfigDir(); err == nil {
		return filepath.Join(base, "flopwire", "server-tls")
	}
	return "flopwire-server-tls"
}

// serverTLS is the resolved transport: config is nil for plain HTTP.
type serverTLS struct {
	mode        string
	config      *tls.Config
	fingerprint string // self-signed only
}

// resolveServerTLS checks the mode against the listen address and loads or
// creates what the mode needs.
func resolveServerTLS(f *tlsFlags, addr string) (*serverTLS, error) {
	mode := strings.ToLower(strings.TrimSpace(f.mode))
	domains := splitList(f.domains)
	if mode == "" || mode == "auto" {
		mode = tlsSelfSigned
		if len(domains) > 0 {
			mode = tlsACME
		}
	}
	switch mode {
	case tlsOff:
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("--addr %q: %w", addr, err)
		}
		if !client.IsLoopbackHost(host) {
			return nil, fmt.Errorf("--tls=off serves plain HTTP and needs a loopback --addr (127.0.0.1, [::1] or localhost), not %q; use --tls=proxy behind a TLS-terminating reverse proxy", addr)
		}
		return &serverTLS{mode: mode}, nil
	case tlsProxy:
		return &serverTLS{mode: mode}, nil
	case tlsSelfSigned:
		cert, fp, err := loadOrCreateSelfSigned(f.dir)
		if err != nil {
			return nil, err
		}
		return &serverTLS{mode: mode, fingerprint: fp, config: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}}, nil
	case tlsACME:
		if len(domains) == 0 {
			return nil, errors.New("--tls=acme needs --domain (FLOPWIRE_DOMAIN)")
		}
		if err := os.MkdirAll(filepath.Join(f.dir, "acme"), 0o700); err != nil {
			return nil, err
		}
		m := &autocert.Manager{Prompt: autocert.AcceptTOS, Cache: autocert.DirCache(filepath.Join(f.dir, "acme")),
			HostPolicy: autocert.HostWhitelist(domains...), Email: f.email}
		cfg := m.TLSConfig()
		cfg.MinVersion = tls.VersionTLS12
		return &serverTLS{mode: mode, config: cfg}, nil
	default:
		return nil, fmt.Errorf("unknown --tls mode %q (auto, self-signed, acme, proxy, off)", f.mode)
	}
}

// announce logs the transport. Proxy mode also writes a banner to stderr:
// it is only safe when a TLS proxy is the sole client of this listener.
func (s *serverTLS) announce(w io.Writer, addr string) {
	switch s.mode {
	case tlsSelfSigned:
		slog.Info("flopwire TLS: self-signed certificate", "fingerprint", s.fingerprint)
		fmt.Fprintf(w, "flopwire: TLS with a self-signed certificate. Devices pin it with:\n  --fingerprint %s\n", s.fingerprint)
	case tlsACME:
		slog.Info("flopwire TLS: ACME certificate (TLS-ALPN-01; the listener must be reachable on public port 443)")
	case tlsProxy:
		slog.Warn("flopwire TLS: DISABLED (--tls=proxy): serving plain HTTP for a TLS-terminating reverse proxy", "addr", addr)
		fmt.Fprintf(w, "\n"+
			"  ************************************************************\n"+
			"  * flopwire is serving PLAIN HTTP on %s (--tls=proxy).\n"+
			"  * Credentials and transcripts cross this listener in clear.\n"+
			"  * Only your TLS-terminating reverse proxy may reach it.\n"+
			"  ************************************************************\n\n", addr)
	case tlsOff:
		slog.Warn("flopwire TLS: off; plain HTTP on loopback only", "addr", addr)
	}
}

// loadOrCreateSelfSigned returns the certificate in dir, creating it on
// first use. The key never leaves dir (mode 0700, key 0600).
func loadOrCreateSelfSigned(dir string) (tls.Certificate, string, error) {
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
	case errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist):
		if err := writeSelfSigned(dir, certPath, keyPath); err != nil {
			return tls.Certificate{}, "", fmt.Errorf("create self-signed certificate in %s: %w", dir, err)
		}
	default:
		return tls.Certificate{}, "", fmt.Errorf("%s must hold both cert.pem and key.pem, or neither (cert: %v, key: %v)", dir, certErr, keyErr)
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("load %s: %w", dir, err)
	}
	return cert, client.Fingerprint(cert.Certificate[0]), nil
}

func writeSelfSigned(dir, certPath, keyPath string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	host := hostname()
	names := []string{"localhost"}
	if host != "" && host != "localhost" {
		names = append(names, host)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "flopwire " + host},
		NotBefore:    now.Add(-time.Hour),
		// Devices pin the certificate itself, so expiry would only force
		// every device to re-pin. It still gets a finite lifetime.
		NotAfter:    now.AddDate(20, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    names,
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	// Key first: a crash between the two leaves a key without a
	// certificate, which the next start reports instead of silently
	// replacing a pinned identity.
	if err = writeFileAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return writeFileAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tls-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(mode); err == nil {
		if _, err = tmp.Write(data); err == nil {
			err = tmp.Sync()
		}
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// fingerprintCmd prints the pin of the server's self-signed certificate.
func fingerprintCmd(args []string) error {
	fs := flag.NewFlagSet("fingerprint", flag.ContinueOnError)
	dir := fs.String("tls-dir", env("FLOPWIRE_TLS_DIR", defaultTLSDir()), "the server's TLS directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(*dir, "cert.pem"), filepath.Join(*dir, "key.pem"))
	if err != nil {
		return fmt.Errorf("no self-signed certificate in %s (start flopwire serve first): %w", *dir, err)
	}
	fmt.Println(client.Fingerprint(cert.Certificate[0]))
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
