package policy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/smtp"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/flashmob/go-guerrilla"
	"github.com/flashmob/go-guerrilla/backends"
	"github.com/flashmob/go-guerrilla/mail"
	_ "github.com/lib/pq"
	"gopkg.in/gomail.v2"
)

type dnsResolver interface {
	LookupMX(context.Context, string) ([]*net.MX, error)
	LookupHost(context.Context, string) ([]string, error)
}

type MainPolicy struct {
	MainConfig
	ctx         context.Context
	db          *sql.DB
	daemon      *guerrilla.Daemon
	wake        chan struct{}
	queueLock   *os.File
	tlsRoots    *x509.CertPool
	resolver    dnsResolver
	dialContext func(context.Context, string, string) (net.Conn, error)
}

func (s *MainPolicy) Processor() backends.Decorator {
	return func(p backends.Processor) backends.Processor {
		return backends.ProcessWith(func(e *mail.Envelope, task backends.SelectTask) (backends.Result, error) {
			result, err := p.Process(e, task)
			if task != backends.TaskSaveMail || err != nil || result.Code() >= 300 {
				return result, err
			}
			queued, err := snapshot(e)
			if err == nil {
				err = queued.Save(s.QueuePath)
			}
			if err != nil {
				return backends.NewResult("451 4.3.0 Unable to persist message"), err
			}
			s.notify()
			return result, nil
		})
	}
}

func (s *MainPolicy) Init(ctx context.Context) (*MainPolicy, error) {
	if err := s.MainConfig.validate(); err != nil {
		return nil, err
	}
	f, err := os.Stat(s.QueuePath)
	if err != nil {
		return nil, err
	}
	if !f.IsDir() {
		return nil, errors.New("QueuePath is not a directory")
	}
	if s.TLSCAFile != "" {
		pem, err := ioutil.ReadFile(s.TLSCAFile)
		if err != nil {
			return nil, err
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("TLSCAFile contains no CA certificates")
		}
		s.tlsRoots = roots
	}
	s.queueLock, err = os.OpenFile(filepath.Join(s.QueuePath, ".queue.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(s.queueLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		s.queueLock.Close()
		return nil, errors.New("queue directory is already in use")
	}
	s.db, err = sql.Open("postgres", s.PgURL)
	if err != nil {
		s.queueLock.Close()
		return nil, errors.New("invalid PostgreSQL configuration")
	}
	s.ctx = ctx
	s.resolver = net.DefaultResolver
	s.wake = make(chan struct{}, 1)
	config := guerrilla.AppConfig{LogFile: "stdout", LogLevel: "info", AllowedHosts: []string{"."}}
	config.BackendConfig = backends.BackendConfig{
		"save_workers_size":  1,
		"save_process":       "HeadersParser|Header|Hasher|Send",
		"log_received_mails": true,
		"primary_mail_host":  s.Hostname,
	}
	config.Servers = []guerrilla.ServerConfig{{
		IsEnabled: true, Hostname: s.Hostname, MaxSize: int64(100 << 20), ListenInterface: s.ListenAddress,
		XClientOn: true,
		TLS:       guerrilla.ServerTLSConfig{PrivateKeyFile: s.KeyFile, PublicKeyFile: s.CertFile, StartTLSOn: true},
	}}
	s.daemon = &guerrilla.Daemon{Config: &config}
	s.daemon.AddProcessor("Send", s.Processor)
	return s, nil
}

func (s *MainPolicy) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// loop grants one owner per file and bounds both memory use and outgoing work.
func (s *MainPolicy) loop() {
	type completion struct {
		name string
		next time.Time
	}
	completed := make(chan completion, s.ProcessCount)
	active := make(map[string]bool)
	deferred := make(map[string]time.Time)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	scan := func() {
		if s.ctx.Err() != nil {
			return
		}
		files, err := ioutil.ReadDir(s.QueuePath)
		if err != nil {
			log.Printf("Read queue: %v", err)
			return
		}
		for _, file := range files {
			if len(active) >= s.ProcessCount {
				break
			}
			name := file.Name()
			if file.IsDir() || filepath.Ext(name) != ".mail" || active[name] || time.Now().Before(deferred[name]) {
				continue
			}
			active[name] = true
			workers.Add(1)
			go func(name string) {
				defer workers.Done()
				var e MyEnvelope
				_, err := e.Load(filepath.Join(s.QueuePath, name))
				if err == nil {
					err = s.handle(&e)
				}
				next := e.nextAttempt()
				if err != nil {
					log.Printf("Queue %s: %v; retained for recovery", name, err)
					next = time.Now().Add(time.Minute)
				}
				completed <- completion{name, next}
			}(name)
		}
	}
	scan()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
			scan()
		case <-ticker.C:
			scan()
		case done := <-completed:
			delete(active, done.name)
			deferred[done.name] = done.next
			if done.next.IsZero() {
				delete(deferred, done.name)
			}
			scan()
		}
	}
}

func (e *MyEnvelope) nextAttempt() time.Time {
	var next time.Time
	for _, d := range e.Deliveries {
		if d.Done || d.Held {
			continue
		}
		if d.NextAttempt.IsZero() {
			return time.Now()
		}
		if next.IsZero() || d.NextAttempt.Before(next) {
			next = d.NextAttempt
		}
	}
	// Held messages stay on disk without repeated delivery attempts.
	if next.IsZero() {
		for _, d := range e.Deliveries {
			if d.Held {
				return time.Now().Add(time.Hour)
			}
		}
	}
	return next
}

func retryDelay(attempt int) time.Duration {
	delays := [...]time.Duration{time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(delays) {
		attempt = len(delays)
	}
	return delays[attempt-1]
}

func permanent(err error) bool {
	var reply *textproto.Error
	return errors.As(err, &reply) && reply.Code/100 == 5
}

func (s *MainPolicy) handle(e *MyEnvelope) error {
	for i := range e.Deliveries {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		d := &e.Deliveries[i]
		if d.Done || d.Held || time.Now().Before(d.NextAttempt) {
			continue
		}
		if !d.Failed {
			destination := &url.URL{Scheme: "default"}
			var err error
			if !e.IsDSN {
				destination, err = s.getDestination(e.MailFrom, d.Recipient)
			}
			if err != nil {
				// Routing failures belong to the administrator, not to the sender.
				d.LastError = err.Error()
				d.NextAttempt = time.Now().Add(retryDelay(1))
				log.Printf("[%s] Route unavailable for %s: %v; queued for retry", e.UUID, d.Recipient.String(), err)
				if err := e.Save(s.QueuePath); err != nil {
					return err
				}
				continue
			}
			err = s.sendMail(e, d.Recipient, destination)
			if err != nil && s.ctx.Err() != nil {
				return s.ctx.Err()
			}
			d.Attempts++
			if err == nil {
				d.Done = true
				d.LastError = ""
				log.Printf("[%s] Delivered to %s", e.UUID, d.Recipient.String())
			} else {
				d.LastError = err.Error()
				d.Failed = permanent(err) || d.Attempts >= s.RetryCount
				if !d.Failed {
					d.NextAttempt = time.Now().Add(retryDelay(d.Attempts))
				}
				log.Printf("[%s] Delivery attempt %d failed: %v", e.UUID, d.Attempts, err)
			}
			if err := e.Save(s.QueuePath); err != nil {
				return err
			}
		}
		if d.Failed {
			if e.IsDSN || e.MailFrom.NullPath || e.MailFrom.IsEmpty() {
				d.Held = true
				log.Printf("[%s] Undeliverable notification held in queue", e.UUID)
			} else {
				// A crash here can replay the notification, but can never discard it.
				if err := s.returnMail(e, i); err != nil {
					return err
				}
				d.Done = true
			}
			if err := e.Save(s.QueuePath); err != nil {
				return err
			}
		}
	}
	for _, d := range e.Deliveries {
		if !d.Done {
			return nil
		}
	}
	return e.Delete(s.QueuePath)
}

func addressCandidates(address mail.Address) []string {
	host := strings.ToLower(address.Host)
	candidates := []string{address.User + "@" + host}
	if i := strings.IndexByte(address.User, '+'); i > 0 {
		candidates = append(candidates, address.User[:i]+"@"+host)
	}
	candidates = append(candidates, host)
	parts := strings.Split(host, ".")
	for i := 1; i < len(parts); i++ {
		candidates = append(candidates, "."+strings.Join(parts[i:], "."))
	}
	return append(candidates, "*")
}

func (s *MainPolicy) parseAddress(from, to mail.Address) ([]string, []string) {
	return addressCandidates(from), addressCandidates(to)
}

func (s *MainPolicy) queryDatabase(from, to []string) (string, error) {
	ctx, cancel := context.WithTimeout(s.ctx, time.Duration(s.SqlTimeout)*time.Second)
	defer cancel()
	for _, f := range from {
		for _, t := range to {
			var result string
			err := s.db.QueryRowContext(ctx, "SELECT destination FROM transport_senders "+
				"WHERE source=$1 AND recipient=$2 AND (region=$3 OR region=$4 OR region=$5) ORDER BY region DESC LIMIT 1",
				f, t, s.ServerMark, s.RegionMark, s.DefaultMark).Scan(&result)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return "", errors.New("routing database query failed")
			}
			return result, nil
		}
	}
	return "default://", nil
}

func (s *MainPolicy) getDestination(from, to mail.Address) (*url.URL, error) {
	f, t := s.parseAddress(from, to)
	destination, err := s.queryDatabase(f, t)
	if err != nil {
		return nil, err
	}
	result, err := url.Parse(destination)
	if err != nil {
		return nil, errors.New("invalid route URL")
	}
	if err := validateDestination(result); err != nil {
		return nil, err
	}
	return result, nil
}

func validateDestination(destination *url.URL) error {
	if destination == nil {
		return errors.New("missing route")
	}
	if destination.Opaque != "" || destination.ForceQuery {
		return errors.New("invalid route URL")
	}
	switch destination.Scheme {
	case "default":
		if destination.Host != "" || destination.User != nil || destination.Path != "" || destination.RawQuery != "" || destination.Fragment != "" {
			return errors.New("invalid default route")
		}
	case "smtp":
		host, port, err := net.SplitHostPort(destination.Host)
		n, portErr := strconv.Atoi(port)
		if err != nil || host == "" || portErr != nil || n < 1 || n > 65535 || destination.Path != "" || destination.RawQuery != "" || destination.Fragment != "" {
			return errors.New("SMTP route requires a host and numeric port")
		}
	default:
		return errors.New("unsupported route scheme")
	}
	return nil
}

func (s *MainPolicy) returnMail(e *MyEnvelope, recipient int) error {
	d := e.Deliveries[recipient]
	id := e.UUID + "-dsn-" + strconv.Itoa(recipient)
	name := filepath.Join(s.QueuePath, id+".mail")
	if _, err := os.Stat(name); err == nil {
		var existing MyEnvelope
		if _, err := existing.Load(name); err != nil {
			return err
		}
		if !existing.IsDSN || len(existing.RcptTo) != 1 || existing.RcptTo[0].String() != e.MailFrom.String() {
			return errors.New("conflicting notification queue file")
		}
		s.notify()
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	re := &MyEnvelope{
		UUID: id, IsDSN: true,
		MailFrom: mail.Address{NullPath: true}, RcptTo: []mail.Address{e.MailFrom},
	}
	m := gomail.NewMessage()
	m.SetAddressHeader("From", "postmaster@"+s.Hostname, "Mail Delivery System")
	m.SetHeader("To", e.MailFrom.String())
	m.SetHeader("Subject", "Mail delivery failed: returning message to sender")
	m.SetHeader("Message-ID", "<"+re.UUID+"@"+s.Hostname+">")
	m.SetDateHeader("Date", time.Now())
	header := strings.ReplaceAll(e.Body, "\r\n", "\n")
	if end := strings.Index(header, "\n\n"); end >= 0 {
		header = header[:end]
	} else {
		header = "(original headers unavailable)"
	}
	if len(header) > 32<<10 {
		header = header[:32<<10]
	}
	m.SetBody("text/plain", fmt.Sprintf("Your message could not be delivered to %s.\n\nDelivery error: %s\n\nOriginal message headers:\n%s\n", d.Recipient.String(), d.LastError, header))
	var body strings.Builder
	if _, err := m.WriteTo(&body); err != nil {
		return err
	}
	re.Body = body.String()
	if err := re.Save(s.QueuePath); err != nil {
		return err
	}
	log.Printf("[%s] Notification queued as %s", e.UUID, re.UUID)
	s.notify()
	return nil
}

func (s *MainPolicy) sendMail(e *MyEnvelope, to mail.Address, destination *url.URL) error {
	if err := validateDestination(destination); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, time.Duration(s.SMTPTimeout)*time.Second)
	defer cancel()
	var addresses []string
	var auth smtp.Auth
	tlsMode := s.MXTLSMode
	if destination.Scheme == "smtp" {
		tlsMode = s.RelayTLSMode
		addresses = []string{destination.Host}
		if destination.User.Username() != "" {
			password, _ := destination.User.Password()
			auth = smtp.PlainAuth("", destination.User.Username(), password, destination.Hostname())
		}
	} else {
		resolver := s.resolver
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		mx, err := resolver.LookupMX(ctx, to.Host)
		if err != nil {
			var dnsErr *net.DNSError
			if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
				return fmt.Errorf("MX lookup: %w", err)
			}
		}
		if len(mx) == 0 {
			// RFC 5321 implicit MX: use the domain's address records when no MX exists.
			if _, err := resolver.LookupHost(ctx, to.Host); err != nil {
				return fmt.Errorf("address lookup: %w", err)
			}
			addresses = []string{net.JoinHostPort(to.Host, "25")}
		} else {
			sort.SliceStable(mx, func(i, j int) bool { return mx[i].Pref < mx[j].Pref })
			for _, record := range mx {
				if record.Host == "." {
					return &textproto.Error{Code: 556, Msg: "recipient domain does not accept mail (null MX)"}
				}
				addresses = append(addresses, net.JoinHostPort(strings.TrimSuffix(record.Host, "."), "25"))
			}
		}
	}
	from := e.MailFrom.String()
	if e.MailFrom.NullPath || e.IsDSN {
		from = ""
	}
	return s.smtp(ctx, addresses, tlsMode, auth, from, []string{to.String()}, []byte(e.Body))
}

func (s *MainPolicy) smtp(ctx context.Context, addresses []string, tlsMode string, auth smtp.Auth, from string, to []string, msg []byte) error {
	if tlsMode != TLSOpportunistic && tlsMode != TLSRequired {
		return errors.New("invalid SMTP TLS mode")
	}
	// Credentials always require an authenticated encrypted connection, even if
	// a legacy unauthenticated relay was configured for opportunistic TLS.
	if auth != nil {
		tlsMode = TLSRequired
	}
	if len(addresses) == 0 || len(to) == 0 {
		return errors.New("empty SMTP destination or recipients")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.SMTPTimeout)*time.Second)
	defer cancel()
	var lastErr error
	dial := s.dialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	for _, address := range addresses {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return errors.New("invalid SMTP address")
		}
		conn, err := dial(ctx, "tcp", address)
		if err != nil {
			lastErr = err
			continue
		}
		err = s.smtpConnection(ctx, conn, host, tlsMode, auth, from, to, msg)
		if err == nil || permanent(err) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

func (s *MainPolicy) smtpConnection(ctx context.Context, conn net.Conn, host, tlsMode string, auth smtp.Auth, from string, to []string, msg []byte) error {
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	return s.smtpTransaction(c, host, tlsMode, auth, from, to, msg)
}

func (s *MainPolicy) smtpTransaction(c *smtp.Client, host, tlsMode string, auth smtp.Auth, from string, to []string, msg []byte) error {
	if err := c.Hello(s.SmtpHostName); err != nil {
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		config := &tls.Config{ServerName: host, RootCAs: s.tlsRoots, MinVersion: tls.VersionTLS12}
		// Opportunistic MX delivery encrypts without requiring Web PKI identity
		// authentication. Required mode (including all AUTH) verifies certificates.
		config.InsecureSkipVerify = tlsMode == TLSOpportunistic
		if err := c.StartTLS(config); err != nil {
			return err
		}
	} else if tlsMode == TLSRequired {
		return errors.New("SMTP server does not offer required STARTTLS")
	}
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, recipient := range to {
		if err := c.Rcpt(recipient); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	// The final DATA response commits delivery. A later QUIT error must not resend it.
	if err := w.Close(); err != nil {
		return err
	}
	return nil
}

func (s *MainPolicy) Run() error {
	defer s.db.Close()
	defer s.queueLock.Close()
	if err := s.daemon.Start(); err != nil {
		return err
	}
	defer s.daemon.Shutdown()
	s.loop()
	return nil
}
