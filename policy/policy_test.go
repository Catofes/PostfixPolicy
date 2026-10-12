package policy

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flashmob/go-guerrilla/mail"
)

func testContext() context.Context { return context.Background() }

type queryFunc func(context.Context, []driver.NamedValue) (string, error)
type testConnector struct{ query queryFunc }

func (c testConnector) Connect(context.Context) (driver.Conn, error) {
	return testConnection{c.query}, nil
}
func (c testConnector) Driver() driver.Driver { return testDriver{c.query} }

type testDriver struct{ query queryFunc }

func (d testDriver) Open(string) (driver.Conn, error) { return testConnection{d.query}, nil }

type testConnection struct{ query queryFunc }

func (c testConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not implemented")
}
func (c testConnection) Close() error              { return nil }
func (c testConnection) Begin() (driver.Tx, error) { return nil, errors.New("not implemented") }
func (c testConnection) QueryContext(ctx context.Context, _ string, args []driver.NamedValue) (driver.Rows, error) {
	value, err := c.query(ctx, args)
	if err != nil {
		return nil, err
	}
	return &testRows{value: value}, nil
}

type testRows struct {
	value string
	done  bool
}

func (r *testRows) Columns() []string { return []string{"destination"} }
func (r *testRows) Close() error      { return nil }
func (r *testRows) Next(values []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	values[0] = r.value
	return nil
}

func testPolicy(t *testing.T, route string) *MainPolicy {
	t.Helper()
	// Queue tests use a plaintext local relay; configuration defaults are tested separately.
	s := &MainPolicy{MainConfig: MainConfig{QueuePath: t.TempDir(), RetryCount: 3, ProcessCount: 2, SqlTimeout: 1, SMTPTimeout: 2, Hostname: "mail.example.com", SmtpHostName: "mail.example.com", MXTLSMode: TLSOpportunistic, RelayTLSMode: TLSOpportunistic}, ctx: context.Background(), wake: make(chan struct{}, 1)}
	s.db = sql.OpenDB(testConnector{query: func(context.Context, []driver.NamedValue) (string, error) { return route, nil }})
	t.Cleanup(func() { s.db.Close() })
	return s
}

func reload(t *testing.T, s *MainPolicy, id string) *MyEnvelope {
	t.Helper()
	var e MyEnvelope
	if _, err := e.Load(filepath.Join(s.QueuePath, id+".mail")); err != nil {
		t.Fatal(err)
	}
	return &e
}

func TestRoutingSpecificityAndDatabaseFailure(t *testing.T) {
	s := testPolicy(t, "default://")
	from, to := s.parseAddress(mail.Address{User: "alice+one+two", Host: "SUB.Example.COM"}, mail.Address{User: "bob+tag", Host: "example.net"})
	want := []string{"alice+one+two@sub.example.com", "alice@sub.example.com", "sub.example.com", ".example.com", ".com", "*"}
	if !reflect.DeepEqual(from, want) || to[1] != "bob@example.net" {
		t.Fatalf("candidates: %v %v", from, to)
	}
	s.db.Close()
	var queries [][2]string
	s.db = sql.OpenDB(testConnector{query: func(ctx context.Context, args []driver.NamedValue) (string, error) {
		f, t := args[0].Value.(string), args[1].Value.(string)
		queries = append(queries, [2]string{f, t})
		if f == "alice@example.com" && t == "*" {
			return "smtp://relay.example:25", nil
		}
		return "", sql.ErrNoRows
	}})
	result, err := s.queryDatabase([]string{"alice@example.com", "example.com"}, []string{"bob@example.net", "*"})
	if err != nil || result != "smtp://relay.example:25" || len(queries) != 2 {
		t.Fatalf("sender priority: %q %v %v", result, err, queries)
	}
	s.db.Close()
	s.db = sql.OpenDB(testConnector{query: func(context.Context, []driver.NamedValue) (string, error) { return "", sql.ErrNoRows }})
	result, err = s.queryDatabase([]string{"*"}, []string{"*"})
	if err != nil || result != "default://" {
		t.Fatalf("unmatched route: %q %v", result, err)
	}
	s.db.Close()
	s.db = sql.OpenDB(testConnector{query: func(context.Context, []driver.NamedValue) (string, error) {
		return "", errors.New("database unavailable")
	}})
	if result, err := s.queryDatabase([]string{"*"}, []string{"*"}); err == nil || result != "" {
		t.Fatal("database failure became MX route")
	}
}

func TestRouteValidation(t *testing.T) {
	for _, raw := range []string{"https://relay:25", "smtp://relay", "smtp://relay:70000", "default://relay", "smtp://relay:25/path", "default:opaque"} {
		destination, err := url.Parse(raw)
		if err == nil {
			err = validateDestination(destination)
		}
		if err == nil {
			t.Fatalf("accepted invalid route %q", raw)
		}
	}
	for _, raw := range []string{"default://", "smtp://relay:25", "smtp://user:secret@[::1]:587"} {
		destination, err := url.Parse(raw)
		if err != nil || validateDestination(destination) != nil {
			t.Fatalf("rejected valid route %q", raw)
		}
	}
}

func TestRecipientsPersistIndependentProgress(t *testing.T) {
	fixture := newSMTPFixture(t, smtpOptions{rejectRecipient: func(recipient string) int {
		if recipient == "<two@example.net>" {
			return 451
		}
		return 250
	}})
	s := testPolicy(t, "smtp://"+fixture.listener.Addr().String())
	e := testEnvelope()
	if err := e.Save(s.QueuePath); err != nil {
		t.Fatal(err)
	}
	if err := s.handle(e); err != nil {
		t.Fatal(err)
	}
	restarted := reload(t, s, e.UUID)
	if !restarted.Deliveries[0].Done || restarted.Deliveries[1].Done || restarted.Deliveries[1].Attempts != 1 || restarted.Deliveries[1].NextAttempt.IsZero() {
		t.Fatalf("incorrect progress: %+v", restarted.Deliveries)
	}
	if len(fixture.messages()) != 1 {
		t.Fatal("wrong successful delivery count")
	}
	if err := s.handle(restarted); err != nil {
		t.Fatal(err)
	}
	if len(fixture.messages()) != 1 {
		t.Fatal("restart repeated completed recipient")
	}
	// Retry only the unfinished recipient against a recovered relay.
	healthy := newSMTPFixture(t, smtpOptions{})
	s.db.Close()
	s.db = sql.OpenDB(testConnector{query: func(context.Context, []driver.NamedValue) (string, error) {
		return "smtp://" + healthy.listener.Addr().String(), nil
	}})
	restarted.Deliveries[1].NextAttempt = time.Time{}
	if err := s.handle(restarted); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.QueuePath, e.UUID+".mail")); !os.IsNotExist(err) {
		t.Fatalf("completed queue file retained: %v", err)
	}
	received := healthy.messages()
	if len(received) != 1 || received[0].recipients[0] != "<two@example.net>" {
		t.Fatalf("wrong retried recipient: %+v", received)
	}
}

func TestFailuresQueueBounceBeforeRemovingOriginal(t *testing.T) {
	for _, code := range []int{451, 550} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			fixture := newSMTPFixture(t, smtpOptions{rejectRecipient: func(string) int { return code }})
			s := testPolicy(t, "smtp://"+fixture.listener.Addr().String())
			s.RetryCount = 1
			e := testEnvelope()
			e.RcptTo = e.RcptTo[:1]
			e.Body = "malformed message without separator"
			if err := e.Save(s.QueuePath); err != nil {
				t.Fatal(err)
			}
			if err := s.handle(e); err != nil {
				t.Fatal(err)
			}
			bounce := reload(t, s, e.UUID+"-dsn-0")
			if !bounce.IsDSN || !bounce.MailFrom.NullPath || bounce.RcptTo[0].String() != e.MailFrom.String() || !strings.Contains(bounce.Body, "original headers unavailable") {
				t.Fatalf("invalid notification: %+v", bounce)
			}
			if _, err := os.Stat(filepath.Join(s.QueuePath, e.UUID+".mail")); !os.IsNotExist(err) {
				t.Fatalf("original not retired: %v", err)
			}
		})
	}
}

func TestDatabaseAndInvalidRoutesRetainOriginal(t *testing.T) {
	for _, route := range []string{"", "https://relay:25"} {
		s := testPolicy(t, route)
		if route == "" {
			s.db.Close()
			s.db = sql.OpenDB(testConnector{query: func(context.Context, []driver.NamedValue) (string, error) { return "", errors.New("database down") }})
		}
		e := testEnvelope()
		if err := e.Save(s.QueuePath); err != nil {
			t.Fatal(err)
		}
		if err := s.handle(e); err != nil {
			t.Fatal(err)
		}
		loaded := reload(t, s, e.UUID)
		if loaded.Deliveries[0].Attempts != 0 || loaded.Deliveries[0].Done || loaded.Deliveries[0].NextAttempt.IsZero() {
			t.Fatal("route failure discarded or attempted delivery")
		}
		files, _ := filepath.Glob(filepath.Join(s.QueuePath, "*.mail"))
		if len(files) != 1 {
			t.Fatal("route failure generated a bounce")
		}
	}
}

func TestFailedNotificationsAreHeldWithoutBounceLoop(t *testing.T) {
	fixture := newSMTPFixture(t, smtpOptions{rejectRecipient: func(string) int { return 550 }})
	s := testPolicy(t, "smtp://"+fixture.listener.Addr().String())
	e := testEnvelope()
	e.RcptTo = e.RcptTo[:1]
	e.MailFrom = mail.Address{NullPath: true}
	if err := e.Save(s.QueuePath); err != nil {
		t.Fatal(err)
	}
	if err := s.handle(e); err != nil {
		t.Fatal(err)
	}
	loaded := reload(t, s, e.UUID)
	if !loaded.Deliveries[0].Held {
		t.Fatal("undeliverable empty-sender mail not held")
	}
	files, _ := filepath.Glob(filepath.Join(s.QueuePath, "*.mail"))
	if len(files) != 1 {
		t.Fatal("generated a bounce to a bounce")
	}
}

func TestBouncePersistenceFailureRetainsParent(t *testing.T) {
	s := testPolicy(t, "default://")
	e := testEnvelope()
	e.RcptTo = e.RcptTo[:1]
	if err := e.Save(s.QueuePath); err != nil {
		t.Fatal(err)
	}
	e.Deliveries[0].Failed = true
	e.Deliveries[0].LastError = "550 rejected"
	if err := e.Save(s.QueuePath); err != nil {
		t.Fatal(err)
	}
	// A directory at the bounce filename makes its atomic rename fail.
	if err := os.Mkdir(filepath.Join(s.QueuePath, e.UUID+"-dsn-0.mail"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.handle(e); err == nil {
		t.Fatal("notification save failure ignored")
	}
	loaded := reload(t, s, e.UUID)
	if loaded.Deliveries[0].Done || !loaded.Deliveries[0].Failed {
		t.Fatal("parent retired without durable notification")
	}
}

type fakeResolver struct {
	mx        []*net.MX
	mxErr     error
	hosts     []string
	hostErr   error
	hostCalls int
}

func (r *fakeResolver) LookupMX(context.Context, string) ([]*net.MX, error) { return r.mx, r.mxErr }
func (r *fakeResolver) LookupHost(context.Context, string) ([]string, error) {
	r.hostCalls++
	return r.hosts, r.hostErr
}

func TestMXResolutionHandlesErrorsImplicitAndNullMX(t *testing.T) {
	s := testPolicy(t, "default://")
	e := testEnvelope()
	for _, mxErr := range []error{nil, &net.DNSError{Err: "no MX", IsNotFound: true}} {
		resolver := &fakeResolver{mxErr: mxErr, hostErr: errors.New("test address lookup failure")}
		s.resolver = resolver
		err := s.sendMail(e, e.RcptTo[0], &url.URL{Scheme: "default"})
		if err == nil || resolver.hostCalls != 1 || !strings.Contains(err.Error(), "address lookup") {
			t.Fatalf("implicit MX not attempted: %v", err)
		}
	}
	resolver := &fakeResolver{mxErr: &net.DNSError{Err: "temporary DNS", IsTemporary: true}}
	s.resolver = resolver
	if err := s.sendMail(e, e.RcptTo[0], &url.URL{Scheme: "default"}); err == nil || resolver.hostCalls != 0 {
		t.Fatal("temporary MX failure bypassed")
	}
	s.resolver = &fakeResolver{mx: []*net.MX{{Host: "."}}}
	if err := s.sendMail(e, e.RcptTo[0], &url.URL{Scheme: "default"}); !permanent(err) {
		t.Fatalf("null MX not rejected permanently: %v", err)
	}
}

func TestSchedulerRecoversQueueAndSkipsCorruption(t *testing.T) {
	fixture := newSMTPFixture(t, smtpOptions{})
	s := testPolicy(t, "smtp://"+fixture.listener.Addr().String())
	e := testEnvelope()
	if err := e.Save(s.QueuePath); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(filepath.Join(s.QueuePath, "corrupt.mail"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx = ctx
	done := make(chan struct{})
	go func() { s.loop(); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(s.QueuePath, e.UUID+".mail")); os.IsNotExist(err) {
			if len(fixture.messages()) != 2 {
				t.Fatal("not all recovered recipients delivered")
			}
			if _, err := os.Stat(filepath.Join(s.QueuePath, "corrupt.mail")); err != nil {
				t.Fatal("corrupt mail was removed")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("queue not recovered")
}

func TestRetryBackoffIsBounded(t *testing.T) {
	if retryDelay(1) != time.Minute || retryDelay(2) != 5*time.Minute || retryDelay(1000) != time.Hour {
		t.Fatal("invalid retry delay")
	}
}

func TestNotificationRecoveryPreservesExistingProgress(t *testing.T) {
	s := testPolicy(t, "default://")
	e := testEnvelope()
	e.RcptTo = e.RcptTo[:1]
	if err := e.Save(s.QueuePath); err != nil {
		t.Fatal(err)
	}
	e.Deliveries[0].Failed = true
	e.Deliveries[0].LastError = "550 rejected"
	if err := s.returnMail(e, 0); err != nil {
		t.Fatal(err)
	}
	bounce := reload(t, s, e.UUID+"-dsn-0")
	bounce.Deliveries[0].Attempts = 2
	bounce.Deliveries[0].NextAttempt = time.Now().Add(time.Minute)
	if err := bounce.Save(s.QueuePath); err != nil {
		t.Fatal(err)
	}
	if err := s.returnMail(e, 0); err != nil {
		t.Fatal(err)
	}
	recovered := reload(t, s, bounce.UUID)
	if recovered.Deliveries[0].Attempts != 2 || !recovered.Deliveries[0].NextAttempt.Equal(bounce.Deliveries[0].NextAttempt) {
		t.Fatal("recovery reset an existing notification")
	}
}

func TestRoutingTimeoutDoesNotBecomeDefaultRoute(t *testing.T) {
	s := testPolicy(t, "default://")
	s.db.Close()
	s.db = sql.OpenDB(testConnector{query: func(ctx context.Context, _ []driver.NamedValue) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	s.ctx = ctx
	if destination, err := s.queryDatabase([]string{"*"}, []string{"*"}); err == nil || destination != "" {
		t.Fatalf("timeout bypassed routing: %q %v", destination, err)
	}
}
