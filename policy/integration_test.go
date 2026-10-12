package policy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"encoding/pem"
	"io/ioutil"
	"net"
	"net/smtp"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSMTPServiceReceivesPersistsAndForwards(t *testing.T) {
	certificate, roots := testCertificate(t)
	relay := newSMTPFixture(t, smtpOptions{certificate: &certificate})
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := ioutil.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	// Reserve an ephemeral port while constructing the server configuration.
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	config, err := (&MainConfig{}).Init(configFile(t, map[string]interface{}{
		"ListenAddress": address, "QueuePath": dir, "KeyFile": keyPath, "CertFile": certPath, "TLSCAFile": certPath, "SMTPTimeout": 2,
	}))
	reserved.Close()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	app, err := (&MainPolicy{MainConfig: *config}).Init(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	app.db.Close()
	app.db = sql.OpenDB(testConnector{query: func(context.Context, []driver.NamedValue) (string, error) {
		return "smtp://" + relay.listener.Addr().String(), nil
	}})
	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("server shutdown: %v", err)
		}
	}()
	var connection net.Conn
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		connection, err = net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	connection.SetDeadline(time.Now().Add(3 * time.Second))
	client, err := smtp.NewClient(connection, "127.0.0.1")
	if err != nil {
		connection.Close()
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Hello("upstream.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := client.StartTLS(&tls.Config{ServerName: "127.0.0.1", RootCAs: roots, MinVersion: tls.VersionTLS12}); err != nil {
		t.Fatal(err)
	}
	if err := client.Mail("sender@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt("one@example.net"); err != nil {
		t.Fatal(err)
	}
	writer, err := client.Data()
	if err != nil {
		t.Fatal(err)
	}
	body := "From: sender@example.com\r\nTo: one@example.net\r\nSubject: integration\r\n\r\noriginal body\r\n"
	if _, err := writer.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		files, err := filepath.Glob(filepath.Join(dir, "*.mail"))
		if err != nil {
			t.Fatal(err)
		}
		received := relay.messages()
		if len(received) == 1 && len(files) == 0 {
			if received[0].from != "<sender@example.com>" || received[0].recipients[0] != "<one@example.net>" {
				t.Fatalf("forwarding changed envelope: %+v", received)
			}
			if !strings.Contains(received[0].body, "Subject: integration\n\noriginal body\n") || !strings.HasPrefix(received[0].body, "Delivered-To:") {
				t.Fatalf("forwarding lost message or delivery headers: %q", received[0].body)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("accepted message was not delivered and retired")
}
