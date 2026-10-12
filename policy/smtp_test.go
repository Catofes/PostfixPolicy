package policy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math/big"
	"net"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturedMail struct {
	from       string
	recipients []string
	body       string
}
type smtpOptions struct {
	certificate     *tls.Certificate
	rejectRecipient func(string) int
	dataCode        int
	closeAfterData  bool
	stall           bool
	rejectSTARTTLS  bool
}
type smtpFixture struct {
	listener    net.Listener
	options     smtpOptions
	mu          sync.Mutex
	received    []capturedMail
	commands    []string
	connections map[net.Conn]bool
	closed      chan struct{}
	workers     sync.WaitGroup
}

func newSMTPFixture(t *testing.T, options smtpOptions) *smtpFixture {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &smtpFixture{listener: listener, options: options, connections: make(map[net.Conn]bool), closed: make(chan struct{})}
	fixture.workers.Add(1)
	go func() {
		defer fixture.workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			fixture.mu.Lock()
			select {
			case <-fixture.closed:
				fixture.mu.Unlock()
				conn.Close()
				return
			default:
			}
			fixture.connections[conn] = true
			fixture.workers.Add(1)
			fixture.mu.Unlock()
			go fixture.serve(conn)
		}
	}()
	t.Cleanup(func() {
		fixture.mu.Lock()
		close(fixture.closed)
		listener.Close()
		for conn := range fixture.connections {
			conn.Close()
		}
		fixture.mu.Unlock()
		fixture.workers.Wait()
	})
	return fixture
}

func (f *smtpFixture) serve(raw net.Conn) {
	defer f.workers.Done()
	defer func() { raw.Close(); f.mu.Lock(); delete(f.connections, raw); f.mu.Unlock() }()
	if f.options.stall {
		<-f.closed
		return
	}
	conn := raw
	reader := textproto.NewReader(bufio.NewReader(conn))
	fmt.Fprint(conn, "220 test ESMTP\r\n")
	var message capturedMail
	for {
		line, err := reader.ReadLine()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.commands = append(f.commands, line)
		f.mu.Unlock()
		switch {
		case strings.HasPrefix(line, "EHLO"):
			fmt.Fprint(conn, "250-test\r\n")
			if f.options.certificate != nil {
				fmt.Fprint(conn, "250-STARTTLS\r\n")
			}
			fmt.Fprint(conn, "250 AUTH PLAIN\r\n")
		case line == "STARTTLS":
			if f.options.rejectSTARTTLS {
				fmt.Fprint(conn, "454 TLS unavailable\r\n")
				continue
			}
			fmt.Fprint(conn, "220 go ahead\r\n")
			secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*f.options.certificate}, MinVersion: tls.VersionTLS12})
			if err := secure.Handshake(); err != nil {
				return
			}
			conn = secure
			reader = textproto.NewReader(bufio.NewReader(conn))
		case strings.HasPrefix(line, "AUTH PLAIN"):
			fmt.Fprint(conn, "235 authenticated\r\n")
		case strings.HasPrefix(line, "MAIL FROM:"):
			message = capturedMail{from: strings.TrimPrefix(line, "MAIL FROM:")}
			fmt.Fprint(conn, "250 sender accepted\r\n")
		case strings.HasPrefix(line, "RCPT TO:"):
			recipient := strings.TrimPrefix(line, "RCPT TO:")
			code := 250
			if f.options.rejectRecipient != nil {
				code = f.options.rejectRecipient(recipient)
			}
			if code == 250 {
				message.recipients = append(message.recipients, recipient)
			}
			fmt.Fprintf(conn, "%d recipient result\r\n", code)
		case line == "DATA":
			fmt.Fprint(conn, "354 send message\r\n")
			body, err := reader.ReadDotBytes()
			if err != nil {
				return
			}
			code := f.options.dataCode
			if code == 0 {
				code = 250
			}
			if code == 250 {
				message.body = string(body)
				f.mu.Lock()
				f.received = append(f.received, message)
				f.mu.Unlock()
			}
			fmt.Fprintf(conn, "%d data result\r\n", code)
			if f.options.closeAfterData {
				return
			}
		case line == "QUIT":
			fmt.Fprint(conn, "221 bye\r\n")
			return
		default:
			fmt.Fprint(conn, "500 unsupported\r\n")
		}
	}
}

func (f *smtpFixture) messages() []capturedMail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedMail(nil), f.received...)
}

func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "SMTP test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

func TestSMTPDeliveryAndDataCommit(t *testing.T) {
	fixture := newSMTPFixture(t, smtpOptions{closeAfterData: true})
	s := &MainPolicy{MainConfig: MainConfig{SmtpHostName: "mail.example.com", SMTPTimeout: 2}}
	body := "Subject: test\r\n\r\n.leading dot\r\nbody\r\n"
	if err := s.smtp(context.Background(), []string{fixture.listener.Addr().String()}, TLSOpportunistic, nil, "", []string{"one@example.net"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	received := fixture.messages()
	if len(received) != 1 || received[0].from != "<>" || received[0].body != "Subject: test\n\n.leading dot\nbody\n" {
		t.Fatalf("unexpected SMTP message: %+v", received)
	}
}

func TestSMTPRequiresTLSAndVerifiesCertificate(t *testing.T) {
	t.Run("required", func(t *testing.T) {
		fixture := newSMTPFixture(t, smtpOptions{})
		s := &MainPolicy{MainConfig: MainConfig{SmtpHostName: "mail.example.com", SMTPTimeout: 2}}
		if err := s.smtp(context.Background(), []string{fixture.listener.Addr().String()}, TLSRequired, nil, "sender@example.com", []string{"one@example.net"}, nil); err == nil {
			t.Fatal("plaintext accepted with required TLS")
		}
		if len(fixture.messages()) != 0 {
			t.Fatal("message sent without TLS")
		}
	})
	certificate, roots := testCertificate(t)
	for _, trusted := range []bool{false, true} {
		name := "untrusted"
		if trusted {
			name = "trusted"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newSMTPFixture(t, smtpOptions{certificate: &certificate})
			s := &MainPolicy{MainConfig: MainConfig{SmtpHostName: "mail.example.com", SMTPTimeout: 2}}
			if trusted {
				s.tlsRoots = roots
			}
			err := s.smtp(context.Background(), []string{fixture.listener.Addr().String()}, TLSRequired, nil, "sender@example.com", []string{"one@example.net"}, []byte("Subject: test\n\nbody"))
			if trusted && err != nil {
				t.Fatal(err)
			}
			if !trusted && (err == nil || len(fixture.messages()) != 0) {
				t.Fatal("untrusted certificate accepted")
			}
		})
	}
}

func TestSMTPEmptyDestinationsAndTimeout(t *testing.T) {
	s := &MainPolicy{MainConfig: MainConfig{SmtpHostName: "mail.example.com", SMTPTimeout: 1}}
	if err := s.smtp(context.Background(), nil, TLSOpportunistic, nil, "sender@example.com", []string{"one@example.net"}, nil); err == nil {
		t.Fatal("empty destinations accepted")
	}
	fixture := newSMTPFixture(t, smtpOptions{stall: true})
	start := time.Now()
	if err := s.smtp(context.Background(), []string{fixture.listener.Addr().String()}, TLSOpportunistic, nil, "sender@example.com", []string{"one@example.net"}, nil); err == nil {
		t.Fatal("stalled connection succeeded")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("SMTP operation exceeded timeout")
	}
}

func TestSMTPContextCancellationClosesStalledConnection(t *testing.T) {
	fixture := newSMTPFixture(t, smtpOptions{stall: true})
	s := &MainPolicy{MainConfig: MainConfig{SmtpHostName: "mail.example.com", SMTPTimeout: 30}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := s.smtp(ctx, []string{fixture.listener.Addr().String()}, TLSOpportunistic, nil, "sender@example.com", []string{"one@example.net"}, nil); err == nil {
		t.Fatal("cancelled connection succeeded")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation waited for the SMTP timeout")
	}
}

func TestAuthenticatedRelayUsesVerifiedTLS(t *testing.T) {
	certificate, roots := testCertificate(t)
	fixture := newSMTPFixture(t, smtpOptions{certificate: &certificate})
	s := testPolicy(t, "default://")
	s.RelayTLSMode = TLSRequired
	s.tlsRoots = roots
	destination, err := url.Parse("smtp://username:password@" + fixture.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	e := testEnvelope()
	if err := s.sendMail(e, e.RcptTo[0], destination); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	commands := append([]string(nil), fixture.commands...)
	fixture.mu.Unlock()
	startedTLS, authenticated := false, false
	for _, command := range commands {
		if command == "STARTTLS" {
			startedTLS = true
		}
		if strings.HasPrefix(command, "AUTH PLAIN") {
			if !startedTLS {
				t.Fatal("credentials sent before TLS")
			}
			authenticated = true
		}
	}
	if !authenticated || len(fixture.messages()) != 1 {
		t.Fatal("authenticated relay did not deliver")
	}
}

func TestOpportunisticTLSEncryptsWithoutCertificateAuthentication(t *testing.T) {
	certificate, _ := testCertificate(t)
	fixture := newSMTPFixture(t, smtpOptions{certificate: &certificate})
	s := &MainPolicy{MainConfig: MainConfig{SmtpHostName: "mail.example.com", SMTPTimeout: 2}}
	// No test CA is trusted: a self-signed certificate must still allow encryption.
	if err := s.smtp(context.Background(), []string{fixture.listener.Addr().String()}, TLSOpportunistic, nil, "sender@example.com", []string{"one@example.net"}, []byte("Subject: test\n\nbody\n")); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	commands := append([]string(nil), fixture.commands...)
	fixture.mu.Unlock()
	startedTLS := false
	for _, command := range commands {
		if command == "STARTTLS" {
			startedTLS = true
		}
	}
	if !startedTLS || len(fixture.messages()) != 1 {
		t.Fatal("opportunistic delivery did not use TLS")
	}
}

func TestAuthenticationCannotWeakenTLSMode(t *testing.T) {
	certificate, _ := testCertificate(t)
	for _, options := range []smtpOptions{{}, {certificate: &certificate}} {
		fixture := newSMTPFixture(t, options)
		s := testPolicy(t, "default://")
		// This relay is explicitly opportunistic, but its credentials force required mode.
		destination, err := url.Parse("smtp://username:password@" + fixture.listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if err := s.sendMail(testEnvelope(), testEnvelope().RcptTo[0], destination); err == nil {
			t.Fatal("authenticated relay accepted plaintext or an untrusted certificate")
		}
		fixture.mu.Lock()
		commands := append([]string(nil), fixture.commands...)
		fixture.mu.Unlock()
		for _, command := range commands {
			if strings.HasPrefix(command, "AUTH") || strings.HasPrefix(command, "MAIL FROM:") {
				t.Fatal("credentials or message sent without verified TLS")
			}
		}
	}
}

func TestRelayModeSelectedIndependentlyFromMX(t *testing.T) {
	fixture := newSMTPFixture(t, smtpOptions{})
	s := testPolicy(t, "default://")
	s.MXTLSMode = TLSOpportunistic
	s.RelayTLSMode = TLSRequired
	destination, err := url.Parse("smtp://" + fixture.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	e := testEnvelope()
	if err := s.sendMail(e, e.RcptTo[0], destination); err == nil {
		t.Fatal("relay inherited the relaxed MX policy")
	}
	s.RelayTLSMode = TLSOpportunistic
	if err := s.sendMail(e, e.RcptTo[0], destination); err != nil {
		t.Fatal(err)
	}
	if len(fixture.messages()) != 1 {
		t.Fatal("explicit legacy relay opt out did not work")
	}
}

func TestVerifiedTLSAuthenticationEvenWhenOpportunisticRequested(t *testing.T) {
	certificate, roots := testCertificate(t)
	fixture := newSMTPFixture(t, smtpOptions{certificate: &certificate})
	s := &MainPolicy{MainConfig: MainConfig{SmtpHostName: "mail.example.com", SMTPTimeout: 2}, tlsRoots: roots}
	auth := smtp.PlainAuth("", "username", "password", "127.0.0.1")
	if err := s.smtp(context.Background(), []string{fixture.listener.Addr().String()}, TLSOpportunistic, auth, "sender@example.com", []string{"one@example.net"}, []byte("Subject: test\n\nbody\n")); err != nil {
		t.Fatal(err)
	}
	if len(fixture.messages()) != 1 {
		t.Fatal("verified authenticated relay did not deliver")
	}
}

func TestMXDeliveryUsesOwnTLSMode(t *testing.T) {
	certificate, _ := testCertificate(t)
	for _, secure := range []bool{false, true} {
		name := "no-starttls"
		if secure {
			name = "untrusted-certificate"
		}
		t.Run(name, func(t *testing.T) {
			options := smtpOptions{}
			if secure {
				options.certificate = &certificate
			}
			fixture := newSMTPFixture(t, options)
			s := testPolicy(t, "default://")
			s.RelayTLSMode = TLSRequired
			s.resolver = &fakeResolver{mx: []*net.MX{{Host: "mx.example.net.", Pref: 10}}}
			var targets []string
			s.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				targets = append(targets, address)
				return (&net.Dialer{}).DialContext(ctx, network, fixture.listener.Addr().String())
			}
			e := testEnvelope()
			destination := &url.URL{Scheme: "default"}
			if err := s.sendMail(e, e.RcptTo[0], destination); err != nil {
				t.Fatal(err)
			}
			if len(targets) != 1 || targets[0] != "mx.example.net:25" || len(fixture.messages()) != 1 {
				t.Fatalf("wrong MX delivery: %v", targets)
			}
			// Opting MX into required mode rejects both missing TLS and wrong/untrusted identity.
			s.MXTLSMode = TLSRequired
			if err := s.sendMail(e, e.RcptTo[0], destination); err == nil {
				t.Fatal("required MX TLS allowed insecure delivery")
			}
			if len(fixture.messages()) != 1 {
				t.Fatal("required MX mode sent rejected message")
			}
		})
	}
}

func TestOpportunisticTLSFailureDoesNotSendPlaintext(t *testing.T) {
	certificate, _ := testCertificate(t)
	fixture := newSMTPFixture(t, smtpOptions{certificate: &certificate, rejectSTARTTLS: true})
	s := testPolicy(t, "default://")
	destination, err := url.Parse("smtp://" + fixture.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	e := testEnvelope()
	if err := e.Save(s.QueuePath); err != nil {
		t.Fatal(err)
	}
	s.db.Close()
	s.db = sql.OpenDB(testConnector{query: func(context.Context, []driver.NamedValue) (string, error) { return destination.String(), nil }})
	if err := s.handle(e); err != nil {
		t.Fatal(err)
	}
	queued := reload(t, s, e.UUID)
	if queued.Deliveries[0].Done || queued.Deliveries[0].Attempts != 1 || queued.Deliveries[0].NextAttempt.IsZero() {
		t.Fatal("TLS failure did not retain the retry state")
	}
	if len(fixture.messages()) != 0 {
		t.Fatal("TLS failure downgraded to plaintext")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	for _, command := range fixture.commands {
		if strings.HasPrefix(command, "MAIL FROM:") {
			t.Fatal("message sent after TLS failed")
		}
	}
}
